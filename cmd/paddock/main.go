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
			initCmd(),
			devCmd(),
			downCmd(),
			attachCmd(),
			execCmd(),
			syncCmd(),
			initLocalCmd(),
			hookBashCmd(),
			hookSessionStartCmd(),
			hookSessionEndCmd(),
			heartbeatCmd(),
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
			&cli.BoolFlag{
				Name:  "no-git-credentials",
				Usage: "do not install the repo's git credentials into the sandbox (the agent can read but not push)",
			},
			&cli.BoolFlag{
				Name:  "no-git-signing",
				Usage: "do not install the repo's commit-signing key into the sandbox (commits are made unsigned)",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			agent := c.Args().First()
			if agent == "" {
				return cli.Exit("which agent? e.g. paddock run claude", 2)
			}
			return runSession(agent, c.Bool("detach"), !c.Bool("no-push"),
				!c.Bool("no-git-credentials"), !c.Bool("no-git-signing"))
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
		Name:  "ls",
		Usage: "list running sessions (use --all to include stopped ones)",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "all",
				Aliases: []string{"a"},
				Usage:   "include stopped sessions (deleted, failed, expired), like docker ps -a",
			},
		},
		Action: func(_ context.Context, c *cli.Command) error { return listSessions(c.Bool("all")) },
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

// provisionSession creates a session for the current directory and performs the
// one-time setup both modes share: upload the workspace and install the
// developer's git identity, credentials, and signing key. It returns the new
// session id. `run` attaches to it in-pod; `dev` binds it for local-harness use.
func provisionSession(agent string, push, gitCreds, gitSigning bool) (string, error) {
	// No "user" field: the server takes the owner from the credential, so
	// claiming one here would be decorative at best and a lie at worst.
	body, _ := json.Marshal(map[string]string{
		"agent":     agent,
		"budget_id": envOr("PADDOCK_BUDGET", "default"),
	})
	req, err := apiRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiDo(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var sess struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &sess); err != nil {
		return "", err
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

	// Git identity is not a secret — the repo's own history already shows it
	// — so this always runs, independent of --no-git-credentials: even an
	// agent that cannot push should still attribute its local commits to the
	// developer instead of git's own fallback identity.
	if err := pushGitIdentity(sess.ID, "."); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not configure git identity: %v\n", err)
	}

	// The repo came up; its credentials should too, or the agent can read the
	// developer's code and do nothing with it — no fetch, no push, no PR.
	// Same reasoning as the workspace: doing this per session by hand is the
	// kind of chore that gets automated badly, so paddock does it once,
	// properly, from the credentials git already holds.
	if gitCreds {
		if err := pushGitCredentials(sess.ID, "."); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not install git credentials: %v\n", err)
			fmt.Fprintf(os.Stderr, "the agent can read the code but not push it\n")
		}
	}

	// If the developer signs their commits, the agent should too, or a repo
	// that requires signatures will reject everything the agent produces.
	// Not fatal: an unsigned commit is worth more than no session.
	if gitSigning {
		if err := pushGitSigning(sess.ID, "."); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not install commit signing: %v\n", err)
			fmt.Fprintf(os.Stderr, "the agent's commits will be unsigned\n")
		}
	}

	return sess.ID, nil
}

// runSession starts an in-pod session: provision it, then attach the agent's
// TUI over the pod (unless --detach).
func runSession(agent string, detach, push, gitCreds, gitSigning bool) error {
	id, err := provisionSession(agent, push, gitCreds, gitSigning)
	if err != nil {
		return err
	}
	if detach {
		fmt.Printf("sandbox is starting; attach with: paddock attach %s\n", id)
		return nil
	}
	return attachSession(id, []string{agent})
}

func listSessions(all bool) error {
	var sessions []struct {
		ID        string    `json:"id"`
		User      string    `json:"user"`
		Agent     string    `json:"agent"`
		BudgetID  string    `json:"budget_id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	path := "/v1/sessions"
	if all {
		path += "?all=1"
	}
	if err := getJSON(path, &sessions); err != nil {
		return err
	}
	if len(sessions) == 0 {
		if all {
			fmt.Println("no sessions yet")
		} else {
			fmt.Println("no running sessions (use --all to include stopped ones)")
		}
		return nil
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
	req, err := apiRequest(http.MethodDelete, "/v1/sessions/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := apiDo(req)
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
	resp, err := apiGet(path)
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
