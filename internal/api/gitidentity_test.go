package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/viktorwelbers/paddock/internal/audit"
)

func TestConfigureGitIdentitySetsNameAndEmail(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	body := `{"name":"Ada Lovelace","email":"ada@example.com"}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-identity", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	cmd := exec.cmds[0]
	if !containsArg(cmd, "Ada Lovelace") || !containsArg(cmd, "ada@example.com") {
		t.Errorf("name/email should be passed as positional args; argv = %v", cmd)
	}
	script := strings.Join(cmd, " ")
	if !strings.Contains(script, `user.name "$1"`) || !strings.Contains(script, `user.email "$2"`) {
		t.Errorf("script should set identity from positionals, not raw values; got %q", script)
	}
	if !hasKind(t, h, "s1", audit.KindGitIdentity) {
		t.Error("configuring git identity must be audited")
	}
}

func TestConfigureGitIdentityAllowsNameOnly(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	body := `{"name":"Ada Lovelace"}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-identity", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
}

func TestConfigureGitIdentityRejectsEmptyBody(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-identity", strings.NewReader(`{}`)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 with neither name nor email", w.Code)
	}
}

func TestConfigureGitIdentityRejectsOversizedField(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	long := strings.Repeat("a", 300)
	body := `{"name":"` + long + `"}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-identity", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized name", w.Code)
	}
}
