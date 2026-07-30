package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// pushGitIdentity carries the developer's git user.name/user.email into the
// sandbox, so a commit made there is attributed to the developer instead of
// whatever git falls back to once nothing is configured. Unlike a credential
// or a signing key, neither value is a secret — the repo's own commit
// history already shows them — so this travels as plain JSON, no age
// handoff needed.
//
// Quiet when there is nothing to send: a laptop with no git identity set
// yields nothing, and `paddock run` carries on rather than failing the
// session over cosmetic commit attribution.
func pushGitIdentity(sessionID, dir string) error {
	name := gitConfigGet(dir, "user.name")
	email := gitConfigGet(dir, "user.email")
	if name == "" && email == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"name": name, "email": email})
	if err != nil {
		return err
	}
	req, err := apiRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/git-identity", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := apiDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	fmt.Printf("git identity configured (%s)\n", identityLabel(name, email))
	return nil
}

func identityLabel(name, email string) string {
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	default:
		return email
	}
}
