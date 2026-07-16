package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/audit"
)

// fakeExecer records what the handler asked the sandbox to run, and plays
// back canned output, so the workspace endpoints can be tested without a
// cluster.
type fakeExecer struct {
	cmds     [][]string
	stdinGot string
	stdout   string
	execErr  error
	waitErr  error
}

func (f *fakeExecer) WaitRunning(context.Context, string) error { return f.waitErr }

func (f *fakeExecer) Exec(_ context.Context, _ string, cmd []string, stdin io.Reader, stdout, _ io.Writer) error {
	f.cmds = append(f.cmds, cmd)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdinGot += string(b)
	}
	if stdout != nil && f.stdout != "" {
		io.WriteString(stdout, f.stdout)
	}
	return f.execErr
}

func newTestHandler(t *testing.T, exec *fakeExecer) *Handler {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sessions, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessions, Audit: aud, Config: Config{Namespace: "paddock"}}
	if exec != nil {
		h.Exec = exec
	}
	if err := sessions.insert(Session{
		ID: "s1", User: "viktor", Agent: "claude", BudgetID: "default",
		Token: "pdk_x", Status: "running", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPushWorkspaceStreamsTarIntoSandbox(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	req := httptest.NewRequest("POST", "/v1/sessions/s1/workspace", strings.NewReader("tar-bytes"))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if len(exec.cmds) != 1 {
		t.Fatalf("commands run = %v, want just the extract", exec.cmds)
	}
	if got := strings.Join(exec.cmds[0], " "); got != "tar -xzf - -C /workspace" {
		t.Errorf("extract command = %q", got)
	}
	if exec.stdinGot != "tar-bytes" {
		t.Errorf("sandbox stdin = %q, want the request body piped through", exec.stdinGot)
	}
	if !strings.Contains(w.Body.String(), `"bytes":9`) {
		t.Errorf("response = %s, want the byte count", w.Body)
	}
	if !hasKind(t, h, "s1", audit.KindWorkspacePush) {
		t.Error("a workspace push must be audited")
	}
}

func TestPushWorkspaceCleanDeletesFirst(t *testing.T) {
	exec := &fakeExecer{}
	h := newTestHandler(t, exec)

	req := httptest.NewRequest("POST", "/v1/sessions/s1/workspace?clean=1", strings.NewReader("x"))
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if len(exec.cmds) != 2 {
		t.Fatalf("commands = %v, want a delete then an extract", exec.cmds)
	}
	if got := strings.Join(exec.cmds[0], " "); got != "find /workspace -mindepth 1 -delete" {
		t.Errorf("clean command = %q", got)
	}
}

func TestPullWorkspaceStreamsTarOut(t *testing.T) {
	exec := &fakeExecer{stdout: "tar-out"}
	h := newTestHandler(t, exec)

	req := httptest.NewRequest("GET", "/v1/sessions/s1/workspace", nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.String() != "tar-out" {
		t.Errorf("body = %q, want the sandbox's tar stream", w.Body)
	}
	if got := strings.Join(exec.cmds[0], " "); got != "tar -czf - -C /workspace ." {
		t.Errorf("archive command = %q", got)
	}
	if !hasKind(t, h, "s1", audit.KindWorkspacePull) {
		t.Error("a workspace pull must be audited — it is data leaving the sandbox")
	}
}

// Without a cluster there is no pod to exec into; saying so beats a 500.
func TestWorkspaceWithoutClusterIsNotImplemented(t *testing.T) {
	h := newTestHandler(t, nil)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/s1/workspace", nil))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
}

func TestWorkspaceUnknownSessionIs404(t *testing.T) {
	h := newTestHandler(t, &fakeExecer{})
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest("GET", "/v1/sessions/nope/workspace", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func hasKind(t *testing.T, h *Handler, session, kind string) bool {
	t.Helper()
	events, err := h.Audit.BySession(session)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
