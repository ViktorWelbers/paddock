// paddock is the developer CLI. It talks to paddock-server over HTTP; the
// server owns the cluster, so this binary needs no kubeconfig for anything
// except `attach` (until the server-side relay lands).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"runtime/debug"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"
)

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

// version reports the module version the binary was built from. `go install
// ...@v0.2.0` stamps this into the build info, so released binaries know what
// they are without any ldflags ceremony; a local `go build` says "dev".
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

func main() {
	cmd := &cli.Command{
		Name:    "paddock",
		Usage:   "run coding agents in governed sandboxes",
		Version: version(),
		Description: "Every session is a locked-down pod: model calls are metered against a\n" +
			"budget, internet access is limited to an allowlist, and the whole lot is\n" +
			"audited. `paddock config set server <url>` once, then `paddock run` from\n" +
			"your project.",
		Commands: []*cli.Command{
			runCmd(),
			attachCmd(),
			pushCmd(),
			pullCmd(),
			lsCmd(),
			rmCmd(),
			budgetCmd(),
			eventsCmd(),
			configCmd(),
		},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runCmd() *cli.Command {
	return &cli.Command{
		Name:      "run",
		Usage:     "spawn a governed session, upload the current directory, and attach",
		ArgsUsage: "<agent>",
		Description: "The working directory is uploaded to the sandbox so the agent has your\n" +
			"code. In a git repo .gitignore decides what travels; .git comes along.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "leave the session running instead of attaching a terminal",
			},
			&cli.BoolFlag{
				Name:  "no-push",
				Usage: "start with an empty /workspace instead of uploading the current directory",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			agent := c.Args().First()
			if agent == "" {
				return cli.Exit("which agent? e.g. paddock run claude", 2)
			}
			return runSession(agent, c.Bool("detach"), !c.Bool("no-push"))
		},
	}
}

func attachCmd() *cli.Command {
	return &cli.Command{
		Name:      "attach",
		Usage:     "attach a terminal to a running session",
		ArgsUsage: "<id> [cmd...]",
		Description: "With no command, runs the session's own agent (claude, pi, ...).\n" +
			"Detaching leaves the session running; re-attach any time.",
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			command := c.Args().Tail()
			if len(command) == 0 {
				// Default to the session's own agent command.
				var sess struct {
					Agent string `json:"agent"`
				}
				if err := getJSON("/v1/sessions/"+id, &sess); err == nil && sess.Agent != "" {
					command = []string{sess.Agent}
				}
			}
			return attachSession(id, command)
		},
	}
}

func pushCmd() *cli.Command {
	return &cli.Command{
		Name:      "push",
		Usage:     "upload a directory into a session's /workspace",
		ArgsUsage: "<id> [dir]",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "clean",
				Usage: "empty /workspace first, so it mirrors the local directory exactly",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			return pushWorkspace(id, argOr(c.Args().Get(1), "."), c.Bool("clean"))
		},
	}
}

func pullCmd() *cli.Command {
	return &cli.Command{
		Name:      "pull",
		Usage:     "download a session's /workspace",
		ArgsUsage: "<id> [dir]",
		Description: "Overwrites files that the archive contains, like a git checkout, and\n" +
			"leaves everything else alone.",
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			return pullWorkspace(id, argOr(c.Args().Get(1), "."))
		},
	}
}

func lsCmd() *cli.Command {
	return &cli.Command{
		Name:   "ls",
		Usage:  "list sessions",
		Action: func(_ context.Context, _ *cli.Command) error { return listSessions() },
	}
}

func rmCmd() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "tear a session down",
		ArgsUsage: "<id>",
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			return deleteSession(id)
		},
	}
}

func budgetCmd() *cli.Command {
	return &cli.Command{
		Name:      "budget",
		Usage:     "show budget headroom",
		ArgsUsage: "[id]",
		Action: func(_ context.Context, c *cli.Command) error {
			return showBudget(argOr(c.Args().First(), "default"))
		},
	}
}

func eventsCmd() *cli.Command {
	return &cli.Command{
		Name:      "events",
		Usage:     "show a session's audit trail",
		ArgsUsage: "<id>",
		Description: "Every model call, tool call, egress attempt (allowed and denied) and\n" +
			"workspace transfer, in order.",
		Action: func(_ context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return cli.Exit("which session? paddock ls shows them", 2)
			}
			return showEvents(id)
		},
	}
}

func argOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func runSession(agent string, detach, push bool) error {
	body, _ := json.Marshal(map[string]string{
		"user":      currentUser(),
		"agent":     agent,
		"budget_id": envOr("PADDOCK_BUDGET", "default"),
	})
	resp, err := http.Post(serverURL()+"/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var sess struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &sess); err != nil {
		return err
	}
	fmt.Printf("session %s created\n", sess.ID)

	// An agent staring at an empty directory is useless, so the working
	// directory goes up by default. A failure here is not fatal: the session
	// exists and is worth attaching to even with an empty workspace.
	if push {
		if err := pushWorkspace(sess.ID, ".", false); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not upload the workspace: %v\n", err)
			fmt.Fprintf(os.Stderr, "the sandbox is empty; retry with: paddock push %s\n", sess.ID)
		}
	}

	if detach {
		fmt.Printf("sandbox is starting; attach with: paddock attach %s\n", sess.ID)
		return nil
	}
	return attachSession(sess.ID, []string{agent})
}

func listSessions() error {
	var sessions []struct {
		ID        string    `json:"id"`
		User      string    `json:"user"`
		Agent     string    `json:"agent"`
		BudgetID  string    `json:"budget_id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := getJSON("/v1/sessions", &sessions); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tUSER\tAGENT\tBUDGET\tSTATUS\tCREATED")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.User, s.Agent, s.BudgetID, s.Status, s.CreatedAt.Local().Format(time.RFC3339))
	}
	return w.Flush()
}

func deleteSession(id string) error {
	req, _ := http.NewRequest(http.MethodDelete, serverURL()+"/v1/sessions/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	fmt.Printf("session %s deleted\n", id)
	return nil
}

func showBudget(id string) error {
	var b struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		LimitUSD float64 `json:"limit_usd"`
		SpentUSD float64 `json:"spent_usd"`
	}
	if err := getJSON("/v1/budgets/"+id, &b); err != nil {
		return err
	}
	fmt.Printf("budget %s (%s): %.4f / %.2f USD spent (%.1f%%)\n",
		b.ID, b.Name, b.SpentUSD, b.LimitUSD, b.SpentUSD/b.LimitUSD*100)
	return nil
}

func showEvents(id string) error {
	var events []struct {
		TS      time.Time      `json:"ts"`
		Kind    string         `json:"kind"`
		Actor   string         `json:"actor"`
		Payload map[string]any `json:"payload"`
	}
	if err := getJSON("/v1/sessions/"+id+"/events", &events); err != nil {
		return err
	}
	for _, e := range events {
		payload, _ := json.Marshal(e.Payload)
		fmt.Printf("%s  %-18s %-10s %s\n", e.TS.Local().Format("15:04:05"), e.Kind, e.Actor, payload)
	}
	return nil
}

func getJSON(path string, v any) error {
	resp, err := http.Get(serverURL() + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
