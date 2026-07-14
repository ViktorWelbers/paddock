package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func openaiFixture(t *testing.T, apiKey string, upstream http.HandlerFunc) (fix, Backends) {
	t.Helper()
	b, sess := newBackends(t, 100)

	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)

	proxy := &OpenAIProxy{Backends: b, Upstream: upURL, APIKey: apiKey}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	return fix{srv: srv, ledger: b.Ledger, audit: b.Audit, token: sess.Token, sessionID: sess.ID}, b
}

// callOpenAI posts the way an OpenAI SDK client does: Bearer auth, /v1 path.
func callOpenAI(t *testing.T, srv *httptest.Server, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestOpenAIJSONUsageMeteredAndBearerSwapped(t *testing.T) {
	var sawAuth, sawPath string
	f, _ := openaiFixture(t, "real-key", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"test-model","usage":{"prompt_tokens":100000,"completion_tokens":100000}}`)
	})

	resp := callOpenAI(t, f.srv, f.token, `{"model":"test-model"}`)
	drain(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if sawAuth != "Bearer real-key" {
		t.Fatalf("upstream saw auth %q, want the real provider key", sawAuth)
	}
	if sawPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", sawPath)
	}
	b, _ := f.ledger.Get("b1")
	if b.SpentUSD != 3 { // 0.1M*10 + 0.1M*20
		t.Fatalf("spent = %v, want 3", b.SpentUSD)
	}
}

func TestOpenAIKeylessUpstreamGetsNoAuth(t *testing.T) {
	var sawAuth string
	sawAuth = "unset"
	f, _ := openaiFixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	drain(t, callOpenAI(t, f.srv, f.token, `{}`))
	if sawAuth != "" {
		t.Fatalf("keyless upstream saw Authorization %q; the session token must never leak upstream", sawAuth)
	}
}

func TestOpenAIStreamUsageForcedAndMetered(t *testing.T) {
	var sawBody []byte
	f, _ := openaiFixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"model":"test-model","choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"model":"test-model","choices":[],"usage":{"prompt_tokens":200000,"completion_tokens":100000}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	// The client streams but does NOT ask for usage; the gateway must force it.
	resp := callOpenAI(t, f.srv, f.token, `{"model":"test-model","stream":true}`)
	drain(t, resp) // reach EOF so the meter settles

	var req map[string]any
	if err := json.Unmarshal(sawBody, &req); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	opts, _ := req["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not forced; upstream saw %s", sawBody)
	}

	b, _ := f.ledger.Get("b1")
	if b.SpentUSD != 4 { // 0.2M*10 + 0.1M*20
		t.Fatalf("spent = %v, want 4", b.SpentUSD)
	}
}

func TestOpenAIBadTokenRejected(t *testing.T) {
	f, _ := openaiFixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached with a bad token")
	})
	resp := callOpenAI(t, f.srv, "pdk_bogus", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
