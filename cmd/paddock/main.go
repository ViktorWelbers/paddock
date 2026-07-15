// paddock is the developer CLI. It talks to paddock-server over HTTP.
//
//	paddock run <agent>       spawn a governed session and attach (--detach to skip)
//	paddock attach <id>       attach a terminal to a running session
//	paddock ls                list sessions
//	paddock rm <id>           tear a session down
//	paddock budget [id]       show budget headroom
//	paddock events <id>       show a session's audit trail
//	paddock config            show/save CLI settings (e.g. the server URL)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"text/tabwriter"
	"time"
)

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runSession(os.Args[2:])
	case "attach":
		if len(os.Args) < 3 {
			usage()
		}
		command := os.Args[3:]
		if len(command) == 0 {
			// Default to the session's own agent command (claude, pi, ...).
			var sess struct {
				Agent string `json:"agent"`
			}
			if err := getJSON("/v1/sessions/"+os.Args[2], &sess); err == nil && sess.Agent != "" {
				command = []string{sess.Agent}
			}
		}
		err = attachSession(os.Args[2], command)
	case "ls":
		err = listSessions()
	case "rm":
		err = withArg(os.Args[2:], deleteSession)
	case "budget":
		id := "default"
		if len(os.Args) > 2 {
			id = os.Args[2]
		}
		err = showBudget(id)
	case "events":
		err = withArg(os.Args[2:], showEvents)
	case "config":
		err = configCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: paddock run <agent> [--detach] | attach <id> [cmd...] | ls | rm <id> | budget [id] | events <id> | config [set server <url>]")
	os.Exit(2)
}

func withArg(args []string, fn func(string) error) error {
	if len(args) < 1 {
		usage()
	}
	return fn(args[0])
}

func runSession(args []string) error {
	detach := false
	agent := ""
	for _, a := range args {
		if a == "--detach" || a == "-d" {
			detach = true
			continue
		}
		if agent == "" {
			agent = a
		}
	}
	if agent == "" {
		usage()
	}
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
