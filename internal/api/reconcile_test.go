package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

// fakeProvisioner stands in for a cluster: `live` is what exists out there,
// which is exactly what drifts from what the store believes.
type fakeProvisioner struct {
	live      []string
	deleted   []string
	deleteErr error
}

func (f *fakeProvisioner) Create(_ context.Context, spec sandbox.Spec) error {
	f.live = append(f.live, spec.SessionID)
	return nil
}

func (f *fakeProvisioner) Delete(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	f.live = slices.DeleteFunc(f.live, func(s string) bool { return s == id })
	return nil
}

func (f *fakeProvisioner) List(context.Context) ([]string, error) {
	return slices.Clone(f.live), nil
}

func sessionID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var sess Session
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func kindsFor(t *testing.T, h *Handler, id string) []string {
	t.Helper()
	events, err := h.Audit.BySession(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// A pod whose session the store forgot — the control plane restarted with
// ephemeral storage — belongs to nobody: it holds its CPU and memory, `ls`
// cannot see it and `rm` 404s. Only kubectl could clear it, which is the
// tool paddock exists to keep out of the developer's hands.
func TestReconcileReapsSandboxWithNoSession(t *testing.T) {
	prov := &fakeProvisioner{live: []string{"ghost-session"}}
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	h.Provisioner = prov

	if err := h.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prov.deleted, "ghost-session") {
		t.Errorf("deleted = %v, want the orphaned sandbox reaped", prov.deleted)
	}
	if k := kindsFor(t, h, "ghost-session"); !slices.Contains(k, audit.KindSandboxReaped) {
		t.Errorf("audit = %v, want a %s event: reaping someone's sandbox is not something to do quietly", k, audit.KindSandboxReaped)
	}
}

// The mirror image: the row says running, the sandbox is gone. Left alone
// the session keeps lying — attach hangs, push fails obscurely, `ls` shows
// it as healthy.
func TestReconcileFailsSessionWithNoSandbox(t *testing.T) {
	prov := &fakeProvisioner{}
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	h.Provisioner = prov

	rec := createSessionReq(t, h, "claude")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body.String())
	}
	id := sessionID(t, rec)

	// The sandbox disappears without the control plane noticing.
	prov.live = nil

	if err := h.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := h.Sessions.get(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != statusFailed {
		t.Errorf("status = %q, want %q: a session with no sandbox must stop claiming to run", sess.Status, statusFailed)
	}
	if k := kindsFor(t, h, id); !slices.Contains(k, audit.KindSessionOrphaned) {
		t.Errorf("audit = %v, want a %s event", k, audit.KindSessionOrphaned)
	}
}

// The common case must be boring: a healthy session and its pod agree, and
// reconciliation touches neither.
func TestReconcileLeavesHealthySessionsAlone(t *testing.T) {
	prov := &fakeProvisioner{}
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	h.Provisioner = prov

	id := sessionID(t, createSessionReq(t, h, "claude"))
	if err := h.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(prov.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing: that was a live session's sandbox", prov.deleted)
	}
	sess, err := h.Sessions.get(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != statusRunning {
		t.Errorf("status = %q, want it left running", sess.Status)
	}
}

// A deleted session's pod lingering (delete raced a crash) is an orphan like
// any other: nothing owns it, so it goes.
func TestReconcileReapsSandboxOfDeletedSession(t *testing.T) {
	prov := &fakeProvisioner{}
	h := testHandler(t, Config{AgentImages: map[string]string{"claude": "img"}, GatewayURL: "http://gw"})
	h.Provisioner = prov

	id := sessionID(t, createSessionReq(t, h, "claude"))
	if err := h.Sessions.setStatus(id, statusDeleted); err != nil {
		t.Fatal(err)
	}

	if err := h.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prov.deleted, id) {
		t.Errorf("deleted = %v, want the deleted session's leftover sandbox reaped", prov.deleted)
	}
}
