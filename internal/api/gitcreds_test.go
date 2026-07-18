package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/viktorwelbers/paddock/internal/audit"
)

func TestGitRecipientReturnsPodKey(t *testing.T) {
	exec := &fakeExecer{stdout: "age1qz9z...examplekey\n"}
	h := newTestHandler(t, exec)

	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/s1/git-recipient", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "age1qz9z...examplekey") {
		t.Errorf("body = %s, want the pod's recipient", w.Body)
	}
	// The keygen must be idempotent: it may create the identity but must not
	// overwrite one that already exists, or it would strand a credential the
	// CLI just encrypted to the old key.
	if got := strings.Join(exec.cmds[0], " "); !strings.Contains(got, "age-keygen -y") {
		t.Errorf("keygen command = %q, want it to derive the recipient", got)
	}
}

// A pod that returns something that is not an age recipient is a malfunction,
// not a key: better a clear 502 than handing the CLI garbage to encrypt to.
func TestGitRecipientRejectsNonAgeOutput(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{stdout: "command not found\n"})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/s1/git-recipient", nil))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestInjectGitCredentialsPipesCiphertextAndAuditsHosts(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	const ciphertext = "-----BEGIN AGE ENCRYPTED FILE-----\nopaque\n-----END AGE ENCRYPTED FILE-----\n"
	body := `{"ciphertext":` + jsonString(ciphertext) + `,"hosts":["github.axa.com"]}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-credentials", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	// The server must move the ciphertext verbatim, and decrypt it in the pod
	// with `age -d` — never in its own process.
	if exec.stdinGot != ciphertext {
		t.Errorf("sandbox stdin = %q, want the ciphertext piped through", exec.stdinGot)
	}
	if got := strings.Join(exec.cmds[0], " "); !strings.Contains(got, "age -d") {
		t.Errorf("install command = %q, want it to decrypt in the pod", got)
	}

	events, err := h.Audit.BySession("s1")
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, e := range events {
		if e.Kind != audit.KindGitCredentials {
			continue
		}
		audited = true
		hosts, _ := e.Payload["hosts"].([]any)
		if len(hosts) != 1 || hosts[0] != "github.axa.com" {
			t.Errorf("audited hosts = %v, want [github.axa.com]", hosts)
		}
	}
	if !audited {
		t.Error("a credential injection must be audited")
	}
}

func TestInjectGitCredentialsRejectsEmptyCiphertext(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	body := `{"hosts":["github.com"]}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-credentials", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a body with no ciphertext", w.Code)
	}
}

// jsonString quotes a string the way encoding/json would, so the test body is
// valid JSON without hand-escaping the age armor's newlines.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
