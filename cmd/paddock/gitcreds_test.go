package main

import (
	"io"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// TestEncryptToRecipientRoundTrips is the crux of the encrypted handoff: what
// the CLI produces must be openable only with the pod's private identity, and
// must come back byte-for-byte. If this holds, the server moving the ciphertext
// never learns the credential.
func TestEncryptToRecipientRoundTrips(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	const plaintext = "https://x-access-token:ghp_secretTOKEN@github.axa.com\n"
	ciphertext, err := encryptToRecipient(plaintext, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, "ghp_secretTOKEN") {
		t.Fatal("the token appears in the ciphertext — it is not actually encrypted")
	}

	// Decrypt the way the pod's `age -d` would.
	dec, err := age.Decrypt(armor.NewReader(strings.NewReader(ciphertext)), id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plaintext {
		t.Errorf("decrypted = %q, want the original credential file", got)
	}
}

// A wrong key must not open it: proves the encryption is bound to the pod's
// recipient, not merely encoded.
func TestEncryptToRecipientRejectsWrongIdentity(t *testing.T) {
	right, _ := age.GenerateX25519Identity()
	wrong, _ := age.GenerateX25519Identity()

	ct, err := encryptToRecipient("secret\n", right.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := age.Decrypt(armor.NewReader(strings.NewReader(ct)), wrong); err == nil {
		t.Error("a different identity decrypted the credential — the key binding is broken")
	}
}

func TestCredentialFileEncodesAndReportsHosts(t *testing.T) {
	// A token with '@' and ':' would otherwise forge a different host line.
	creds := []gitCredential{
		{Protocol: "https", Host: "github.axa.com", Username: "x-access-token", Password: "ghp_a:b@c"},
	}
	file, hosts, err := credentialFile(creds)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0] != "github.axa.com" {
		t.Errorf("hosts = %v", hosts)
	}
	// The password's specials must be percent-encoded, and the host must be
	// exactly the one asked for — not one the raw token could smuggle in.
	if !strings.Contains(file, "ghp_a%3Ab%40c@github.axa.com") {
		t.Errorf("credential line = %q, want the password percent-encoded", file)
	}
}

// A newline in any field could forge extra credential entries; it must be
// refused rather than encoded.
func TestCredentialFileRejectsNewlineInjection(t *testing.T) {
	creds := []gitCredential{
		{Host: "github.com", Username: "u", Password: "tok\nhttps://evil:x@other.com"},
	}
	if _, _, err := credentialFile(creds); err == nil {
		t.Error("a newline in a credential field must be rejected")
	}
}

func TestHTTPSHostSkipsSSHAndStripsEmbeddedCreds(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantOK   bool
	}{
		{"https://github.com/team/repo.git", "github.com", true},
		{"https://x-access-token:tok@github.axa.com/team/repo.git", "github.axa.com", true},
		{"http://internal.git/team/repo.git", "internal.git", true},
		{"git@github.com:team/repo.git", "", false},
		{"ssh://git@github.com/team/repo.git", "", false},
		{"/local/path", "", false},
	}
	for _, c := range cases {
		h, ok := httpsHost(c.url)
		if ok != c.wantOK || h.Host != c.wantHost {
			t.Errorf("httpsHost(%q) = (%q, %v), want (%q, %v)", c.url, h.Host, ok, c.wantHost, c.wantOK)
		}
	}
}
