package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/viktorwelbers/paddock/internal/audit"
)

func TestConfigureSSHSigningInstallsKeyAndConfig(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	body := `{"format":"ssh","key_id":"SHA256:abc","commit_sign":true,"tag_sign":false,` +
		`"ciphertext":` + jsonString("-----BEGIN AGE ENCRYPTED FILE-----\nx\n-----END AGE ENCRYPTED FILE-----\n") + `}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-signing", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	script := strings.Join(exec.cmds[0], " ")
	if !strings.Contains(script, "age -d") || !strings.Contains(script, "gpg.format ssh") {
		t.Errorf("ssh install should decrypt and set gpg.format ssh; got %q", script)
	}
	// commit.gpgsign must arrive as a positional arg ("true"), not baked into
	// the script — that is what keeps a hostile value from being shell.
	if last := exec.cmds[0]; last[len(last)-2] != "true" || last[len(last)-1] != "false" {
		t.Errorf("commit/tag flags = %v, want positional true false", last[len(last)-2:])
	}
	if !hasKind(t, h, "s1", audit.KindGitSigning) {
		t.Error("configuring signing must be audited")
	}
}

func TestConfigureGPGSigningNeedsAValidKeyID(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	// A key id with a shell metacharacter must be refused outright.
	body := `{"format":"openpgp","key_id":"$(rm -rf /)","commit_sign":true,` +
		`"ciphertext":` + jsonString("ct") + `}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-signing", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a bad key id", w.Code)
	}
}

func TestConfigureGPGSigningPassesKeyIDAsArgument(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	body := `{"format":"openpgp","key_id":"ABCD1234","commit_sign":true,"tag_sign":true,` +
		`"ciphertext":` + jsonString("ct") + `}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-signing", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	cmd := exec.cmds[0]
	script := strings.Join(cmd, " ")
	if !strings.Contains(script, "gpg --batch --import") {
		t.Errorf("gpg install should import the secret subkey; got %q", script)
	}
	// The key id must be an argv element, not interpolated into the script.
	if !containsArg(cmd, "ABCD1234") {
		t.Errorf("key id should be passed as a positional arg; argv = %v", cmd)
	}
	// And the script must reference the positional, never the literal id.
	if !strings.Contains(script, `user.signingkey "$1"`) {
		t.Errorf("script should set user.signingkey from $1, not the raw id; got %q", script)
	}
}

func TestConfigureSigningRejectsUnknownFormat(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	body := `{"format":"x509","key_id":"a","ciphertext":` + jsonString("ct") + `}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-signing", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unsupported format", w.Code)
	}
}

func TestConfigureSigningRejectsEmptyCiphertext(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	body := `{"format":"ssh","commit_sign":true}`
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/sessions/s1/git-signing", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 with no key material", w.Code)
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}
