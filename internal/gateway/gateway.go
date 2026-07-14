// Package gateway implements the data plane's model-API side: a reverse
// proxy that authenticates sandbox session tokens, swaps in the real
// provider key, meters token usage from responses (JSON and SSE), and
// debits the budget ledger.
//
// Budget enforcement model: headroom is checked before each call (hard
// stop, HTTP 402) and the debit lands after the response, so a session can
// overshoot by at most one call — acceptable for cost control and it keeps
// the proxy out of the request hot path.
package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/viktorwelbers/paddock/internal/api"
	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/budget"
)

// Usage is what metering extracts from one model response.
type Usage struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// Backends bundles what every provider proxy needs to authenticate a
// session token, check headroom, and bill usage.
type Backends struct {
	Sessions *api.Store
	Ledger   *budget.Ledger
	Audit    *audit.Store
}

// authorize authenticates the session token and enforces the pre-call
// budget hard stop. On failure it has already written the error response.
func (b Backends) authorize(w http.ResponseWriter, r *http.Request) (api.Session, bool) {
	token := sessionToken(r)
	if token == "" {
		http.Error(w, "missing session token", http.StatusUnauthorized)
		return api.Session{}, false
	}
	sess, err := b.Sessions.ByToken(token)
	if err != nil {
		http.Error(w, "unknown or expired session token", http.StatusUnauthorized)
		return api.Session{}, false
	}

	remaining, err := b.Ledger.Remaining(sess.BudgetID)
	if err != nil {
		http.Error(w, "budget lookup failed", http.StatusInternalServerError)
		return api.Session{}, false
	}
	if remaining <= 0 {
		_ = b.Audit.Append(audit.Event{
			SessionID: sess.ID, Actor: sess.User, Kind: audit.KindBudgetExhausted,
			Payload: map[string]any{"budget_id": sess.BudgetID},
		})
		http.Error(w, "budget exhausted", http.StatusPaymentRequired)
		return api.Session{}, false
	}
	return sess, true
}

// AnthropicProxy fronts the Anthropic Messages API for sandboxes.
type AnthropicProxy struct {
	Backends
	Upstream *url.URL // e.g. https://api.anthropic.com
	APIKey   string   // real provider key; sandboxes never see it
}

func (p *AnthropicProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := p.authorize(w, r)
	if !ok {
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.Upstream)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/anthropic")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("x-api-key", p.APIKey)
		},
		ModifyResponse: func(resp *http.Response) error {
			return p.meter(resp, sess, parseAnthropicJSONUsage, parseAnthropicSSELine)
		},
	}
	proxy.ServeHTTP(w, r)
}

// sessionToken accepts the token wherever an Anthropic client would put a
// key: x-api-key or Authorization: Bearer.
func sessionToken(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); v != r.Header.Get("Authorization") {
		return v
	}
	return ""
}

// meter hooks usage extraction into a proxied response: JSON bodies are
// parsed in place, SSE streams are tapped as they relay. The parsers are
// provider-specific; billing is shared.
func (b Backends) meter(resp *http.Response, sess api.Session, parseJSON func([]byte) (Usage, bool), parseSSELine func(string, *Usage)) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil // errors cost nothing
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		if u, ok := parseJSON(body); ok {
			b.settle(sess, u)
		}
	case strings.HasPrefix(ct, "text/event-stream"):
		resp.Body = newSSEMeter(resp.Body, parseSSELine, func(u Usage) { b.settle(sess, u) })
	}
	return nil
}

// settle debits the ledger and writes the audit trail for one model call.
func (b Backends) settle(sess api.Session, u Usage) {
	res, err := b.Ledger.Debit(sess.BudgetID, u.Model, u.InputTokens, u.OutputTokens)
	if err != nil {
		return // storage failure; the pre-call check still bounds exposure
	}
	payload := map[string]any{
		"model":         u.Model,
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"cost_usd":      res.CostUSD,
	}
	_ = b.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindModelCall, Payload: payload,
	})
	if res.Exceeded {
		// The response already went through (post-paid); the hard stop
		// lands on the session's next call. Record the breach.
		_ = b.Audit.Append(audit.Event{
			SessionID: sess.ID, Actor: sess.User, Kind: audit.KindBudgetExhausted, Payload: payload,
		})
	}
	for _, warn := range res.Warnings {
		_ = b.Audit.Append(audit.Event{
			SessionID: sess.ID, Actor: sess.User, Kind: audit.KindBudgetWarn,
			Payload: map[string]any{"warning": warn},
		})
	}
}

func parseAnthropicJSONUsage(body []byte) (Usage, bool) {
	var msg struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return Usage{}, false
	}
	if msg.Usage.InputTokens == 0 && msg.Usage.OutputTokens == 0 {
		return Usage{}, false
	}
	return Usage{Model: msg.Model, InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens}, true
}

// sseMeter relays an SSE stream unchanged while a provider-specific line
// parser accumulates usage. onDone fires once, when the stream ends.
type sseMeter struct {
	src    io.ReadCloser
	lines  bytes.Buffer
	usage  Usage
	parse  func(string, *Usage)
	onDone func(Usage)
	done   bool
}

func newSSEMeter(src io.ReadCloser, parse func(string, *Usage), onDone func(Usage)) *sseMeter {
	return &sseMeter{src: src, parse: parse, onDone: onDone}
}

func (s *sseMeter) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		s.lines.Write(p[:n])
		s.drainLines()
	}
	if err != nil {
		s.finish()
	}
	return n, err
}

func (s *sseMeter) Close() error {
	s.finish()
	return s.src.Close()
}

func (s *sseMeter) finish() {
	if s.done {
		return
	}
	s.done = true
	if s.usage.InputTokens > 0 || s.usage.OutputTokens > 0 {
		s.onDone(s.usage)
	}
}

func (s *sseMeter) drainLines() {
	for {
		line, err := s.lines.ReadString('\n')
		if err != nil {
			// Partial line: keep it buffered for the next Read.
			s.lines.Reset()
			s.lines.WriteString(line)
			return
		}
		s.parse(strings.TrimRight(line, "\r\n"), &s.usage)
	}
}

// parseAnthropicSSELine extracts usage from an Anthropic stream:
// message_start carries the model and input tokens, message_delta carries
// the cumulative output tokens.
func parseAnthropicSSELine(line string, u *Usage) {
	data, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		u.Model = ev.Message.Model
		u.InputTokens = ev.Message.Usage.InputTokens
	case "message_delta":
		// Cumulative per the Anthropic streaming spec: take the last value.
		if ev.Usage.OutputTokens > 0 {
			u.OutputTokens = ev.Usage.OutputTokens
		}
	}
}
