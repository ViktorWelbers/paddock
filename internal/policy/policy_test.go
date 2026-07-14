package policy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testPolicy = `
package paddock.authz

import rego.v1

default allow := false

deny contains msg if {
	input.kind == "tool_call"
	input.tool == "curl"
	msg := "curl is not allowed"
}

allow if count(deny) == 0
`

func testEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "authz.rego"), []byte(testPolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDeniedToolCall(t *testing.T) {
	e := testEngine(t)
	d, err := e.Evaluate(context.Background(), Input{
		Kind: "tool_call", User: "viktor", Session: "s1", Tool: "curl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow {
		t.Fatal("curl should be denied")
	}
	if len(d.Reasons) != 1 || d.Reasons[0] != "curl is not allowed" {
		t.Fatalf("reasons = %v", d.Reasons)
	}
}

func TestAllowedToolCall(t *testing.T) {
	e := testEngine(t)
	d, err := e.Evaluate(context.Background(), Input{
		Kind: "tool_call", User: "viktor", Session: "s1", Tool: "grep",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow {
		t.Fatalf("grep should be allowed; reasons = %v", d.Reasons)
	}
}

// TestShippedPolicies keeps the example policies in policies/ loadable and
// sane, so the repo's out-of-the-box config can't rot.
func TestShippedPolicies(t *testing.T) {
	e, err := NewEngine(context.Background(), "../../policies")
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.Evaluate(context.Background(), Input{Kind: "tool_call", Tool: "wget"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow {
		t.Fatal("shipped policy should deny wget")
	}
	d, err = e.Evaluate(context.Background(), Input{Kind: "mcp_call", Server: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow {
		t.Fatalf("shipped policy should allow a named MCP server; reasons = %v", d.Reasons)
	}
}
