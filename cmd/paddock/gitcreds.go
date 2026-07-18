package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// gitCredential mirrors git's own vocabulary. It never leaves this machine in
// the clear: the password is folded into the credential file, which is then
// encrypted to the session pod's key before anything is sent.
type gitCredential struct {
	Protocol string
	Host     string
	Username string
	Password string
}

// harvestGitCredentials collects the credentials for whichever git hosts this
// repo actually talks to.
//
// It asks git rather than reading files or inventing a paddock-specific
// setting, and that is the entire design: `git credential fill` consults the
// same helper chain a plain `git push` would — the macOS keychain, libsecret,
// a manager on Windows, ~/.git-credentials, whatever the developer already
// has working. So there is nothing to configure, nothing to keep in sync, and
// no second place for a credential to go stale. If `git push` works on the
// laptop, it works in the sandbox.
//
// Silence is deliberate. A repo with no remote, an ssh remote, or a host with
// nothing stored yields nothing, and `paddock run` carries on: plenty of
// sessions never touch a remote, and refusing to start one over a credential
// nobody asked for would be its own bug.
func harvestGitCredentials(dir string) []gitCredential {
	var out []gitCredential
	seen := map[string]bool{}
	for _, host := range gitRemoteHosts(dir) {
		key := host.Protocol + "://" + host.Host
		if seen[key] {
			continue
		}
		seen[key] = true
		if c, ok := fillCredential(dir, host.Protocol, host.Host); ok {
			out = append(out, c)
		}
	}
	return out
}

type remoteHost struct{ Protocol, Host string }

// gitRemoteHosts reports the https hosts this repo has remotes on.
//
// ssh remotes are skipped: they authenticate with a key and an agent, which
// is a different mechanism than a credential — see the roadmap.
func gitRemoteHosts(dir string) []remoteHost {
	cmd := exec.Command("git", "-C", dir, "config", "--get-regexp", `^remote\..*\.url$`)
	out, err := cmd.Output()
	if err != nil {
		return nil // not a repo, or no remotes
	}
	var hosts []remoteHost
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		_, rawURL, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		h, ok := httpsHost(rawURL)
		if !ok {
			continue
		}
		hosts = append(hosts, h)
	}
	return hosts
}

// httpsHost extracts the host from a remote URL, for http(s) remotes only.
// Any credential already embedded in the URL is ignored here: git will hand
// it back through `credential fill` if it is the one in use, and reading it
// out of the URL ourselves would just be a second way to get it wrong.
func httpsHost(rawURL string) (remoteHost, bool) {
	protocol, rest, ok := strings.Cut(rawURL, "://")
	if !ok || (protocol != "https" && protocol != "http") {
		return remoteHost{}, false // ssh, git://, or a local path
	}
	if _, after, found := strings.Cut(rest, "@"); found {
		rest = after
	}
	host, _, _ := strings.Cut(rest, "/")
	if host == "" {
		return remoteHost{}, false
	}
	return remoteHost{Protocol: protocol, Host: host}, true
}

// fillCredential asks git for one host's credential.
//
// The prompts are disabled on purpose: `paddock run` is not the moment to
// discover that a host needs an interactive login, and a GUI askpass popping
// up mid-command would be worse than doing without.
func fillCredential(dir, protocol, host string) (gitCredential, bool) {
	cmd := exec.Command("git", "-C", dir, "credential", "fill")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("protocol=%s\nhost=%s\n\n", protocol, host))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "SSH_ASKPASS=true")
	out, err := cmd.Output()
	if err != nil {
		return gitCredential{}, false // nothing stored for this host
	}

	c := gitCredential{Protocol: protocol, Host: host}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "username":
			c.Username = value
		case "password":
			c.Password = value
		}
	}
	// An empty password means the helper chain came up dry rather than
	// refusing outright; there is nothing to send.
	if c.Password == "" {
		return gitCredential{}, false
	}
	return c, true
}

// credentialFile renders git's credential-store format and reports the hosts
// covered. This runs on the developer's machine, on plaintext that never
// leaves it un-encrypted; the server only ever sees the ciphertext of the
// result plus the returned host list.
//
// Each line is a URL, so every component is percent-encoded: tokens
// legitimately contain '@' and ':', and a raw one would silently produce a
// line git parses as a different host entirely.
func credentialFile(creds []gitCredential) (string, []string, error) {
	var b strings.Builder
	hosts := make([]string, 0, len(creds))
	for i, c := range creds {
		if c.Host == "" || c.Password == "" {
			return "", nil, fmt.Errorf("credentials[%d]: host and password are required", i)
		}
		protocol := c.Protocol
		if protocol == "" {
			protocol = "https"
		}
		// A newline would let one credential forge extra entries in the file.
		if strings.ContainsAny(c.Host+c.Username+c.Password, "\r\n") {
			return "", nil, fmt.Errorf("credentials[%d]: newline in a credential field", i)
		}
		fmt.Fprintf(&b, "%s://%s:%s@%s\n", protocol,
			url.QueryEscape(c.Username), url.QueryEscape(c.Password), c.Host)
		hosts = append(hosts, c.Host)
	}
	return b.String(), hosts, nil
}

// gitRecipient fetches the session pod's age recipient. The pod generates the
// keypair on first ask and keeps the private half; this public half is all we
// need to encrypt to it.
func gitRecipient(sessionID string) (*age.X25519Recipient, error) {
	resp, err := apiGet("/v1/sessions/" + sessionID + "/git-recipient")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw))
	}
	var body struct {
		Recipient string `json:"recipient"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return age.ParseX25519Recipient(body.Recipient)
}

// encryptToRecipient armors the credential file under the pod's public key, so
// what crosses the wire and the exec channel is opaque to everything but the
// pod. Armor keeps it ASCII, which `age -d` reads back without a flag.
func encryptToRecipient(plaintext string, r age.Recipient) (string, error) {
	var out bytes.Buffer
	armorW := armor.NewWriter(&out)
	encW, err := age.Encrypt(armorW, r)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(encW, plaintext); err != nil {
		return "", err
	}
	if err := encW.Close(); err != nil {
		return "", err
	}
	if err := armorW.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// sendGitCredentials posts the ciphertext and the (non-secret) host list.
func sendGitCredentials(sessionID string, ciphertext string, hosts []string) error {
	body, err := json.Marshal(map[string]any{"ciphertext": ciphertext, "hosts": hosts})
	if err != nil {
		return err
	}
	req, err := apiRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/git-credentials", bytes.NewReader(body))
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
	return nil
}

// pushGitCredentials harvests, encrypts to the pod's key, and installs, in one
// step — reporting what it did. Hosts, never secrets: this prints to a
// terminal, and the secret has already been sealed.
func pushGitCredentials(sessionID, dir string) error {
	creds := harvestGitCredentials(dir)
	if len(creds) == 0 {
		return nil // nothing stored for this repo's remotes
	}
	plaintext, hosts, err := credentialFile(creds)
	if err != nil {
		return err
	}
	recipient, err := gitRecipient(sessionID)
	if err != nil {
		return err
	}
	ciphertext, err := encryptToRecipient(plaintext, recipient)
	if err != nil {
		return err
	}
	if err := sendGitCredentials(sessionID, ciphertext, hosts); err != nil {
		return err
	}
	slices.Sort(hosts)
	fmt.Printf("git credentials installed (encrypted) for %s\n", strings.Join(hosts, ", "))
	return nil
}
