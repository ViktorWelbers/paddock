// Package policy embeds OPA and evaluates Rego policies on every tool and
// MCP call. Policies are plain .rego files in a directory: reviewable in
// git, testable with `opa test`, and compatible with the Rego workflows
// platform teams already run for Gatekeeper/Conftest.
package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-policy-agent/opa/v1/rego"
)

// Input is the stable decision document passed to Rego as `input`.
type Input struct {
	Kind    string         `json:"kind"` // "tool_call" | "mcp_call"
	User    string         `json:"user"`
	Session string         `json:"session"`
	Tool    string         `json:"tool,omitempty"`   // tool_call
	Server  string         `json:"server,omitempty"` // mcp_call
	Args    map[string]any `json:"args,omitempty"`
}

type Decision struct {
	Allow   bool
	Reasons []string // populated from the policy's `deny` set on denial
}

// Engine holds a prepared query over data.paddock.authz.
type Engine struct {
	query rego.PreparedEvalQuery
}

// NewEngine loads every .rego file under dir and prepares the authz query.
func NewEngine(ctx context.Context, dir string) (*Engine, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Kubernetes ConfigMap mounts keep hidden `..data`/`..<timestamp>`
		// copies of every file; loading those too would double every rule.
		if d.IsDir() && path != dir && d.Name()[0] == '.' {
			return filepath.SkipDir
		}
		if !d.IsDir() && filepath.Ext(path) == ".rego" && d.Name()[0] != '.' {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan policy dir %q: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .rego files found in %q", dir)
	}

	q, err := rego.New(
		rego.Query("data.paddock.authz"),
		rego.Load(files, nil),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare policies: %w", err)
	}
	return &Engine{query: q}, nil
}

// Evaluate runs the authz package against the input. Fail closed: any
// evaluation problem or missing `allow` rule is a denial.
func (e *Engine) Evaluate(ctx context.Context, in Input) (Decision, error) {
	rs, err := e.query.Eval(ctx, rego.EvalInput(in))
	if err != nil {
		return Decision{}, fmt.Errorf("policy eval: %w", err)
	}
	d := Decision{}
	if len(rs) == 0 {
		return d, nil
	}
	pkg, ok := rs[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return d, nil
	}
	if allow, ok := pkg["allow"].(bool); ok {
		d.Allow = allow
	}
	if denies, ok := pkg["deny"].([]any); ok {
		for _, r := range denies {
			if msg, ok := r.(string); ok {
				d.Reasons = append(d.Reasons, msg)
			}
		}
	}
	return d, nil
}
