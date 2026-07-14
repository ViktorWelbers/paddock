package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// maxRequestBody bounds how much of a completion request the gateway will
// buffer when forcing usage reporting; larger bodies pass through untouched.
const maxRequestBody = 32 << 20

// OpenAIProxy fronts an OpenAI-compatible chat completions API (vLLM,
// llama.cpp, LM Studio, ...) for sandboxes. Same contract as the Anthropic
// proxy: the session token arrives as the API key, the real key (if the
// upstream needs one at all — self-hosted servers often don't) is swapped
// in here.
type OpenAIProxy struct {
	Backends
	Upstream  *url.URL          // e.g. https://vllm.internal — the client's /v1/... path passes through
	APIKey    string            // optional; empty means the upstream is keyless
	Transport http.RoundTripper // optional; e.g. a transport trusting a private CA
}

func (p *OpenAIProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sess, ok := p.authorize(w, r)
	if !ok {
		return
	}
	// Metering must not be optional: force the final SSE chunk to carry
	// usage even when the client didn't ask for it.
	forceStreamUsage(r)

	proxy := &httputil.ReverseProxy{
		Transport: p.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(p.Upstream)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/openai")
			pr.Out.Header.Del("x-api-key")
			if p.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+p.APIKey)
			} else {
				pr.Out.Header.Del("Authorization")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			return p.meter(resp, sess, parseOpenAIJSONUsage, parseOpenAISSELine)
		},
	}
	proxy.ServeHTTP(w, r)
}

// forceStreamUsage rewrites a streaming completion request to set
// stream_options.include_usage, so a client can't opt out of metering by
// omitting it. Anything that doesn't parse as a streaming JSON request is
// forwarded unchanged.
func forceStreamUsage(r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return
	}
	if r.ContentLength < 0 || r.ContentLength > maxRequestBody {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		return
	}
	var req map[string]any
	if json.Unmarshal(body, &req) == nil {
		if stream, _ := req["stream"].(bool); stream {
			opts, _ := req["stream_options"].(map[string]any)
			if opts == nil {
				opts = map[string]any{}
			}
			opts["include_usage"] = true
			req["stream_options"] = opts
			if rewritten, err := json.Marshal(req); err == nil {
				body = rewritten
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
}

func parseOpenAIJSONUsage(body []byte) (Usage, bool) {
	var msg struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return Usage{}, false
	}
	if msg.Usage.PromptTokens == 0 && msg.Usage.CompletionTokens == 0 {
		return Usage{}, false
	}
	return Usage{Model: msg.Model, InputTokens: msg.Usage.PromptTokens, OutputTokens: msg.Usage.CompletionTokens}, true
}

// parseOpenAISSELine extracts usage from an OpenAI-style stream: with
// include_usage set, the final chunk before [DONE] carries the totals.
func parseOpenAISSELine(line string, u *Usage) {
	data, ok := strings.CutPrefix(line, "data: ")
	if !ok || data == "[DONE]" {
		return
	}
	var chunk struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Model != "" {
		u.Model = chunk.Model
	}
	if chunk.Usage != nil {
		u.InputTokens = chunk.Usage.PromptTokens
		u.OutputTokens = chunk.Usage.CompletionTokens
	}
}
