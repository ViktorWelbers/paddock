package egress

import (
	"testing"
)

func TestMatchGroups(t *testing.T) {
	a, err := normalize(&Allowlist{Groups: map[string][]string{
		"package_registries": {"pypi.org", "files.pythonhosted.org"},
		"github":             {"github.com", "*.github.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		host string
		want string // first group expected, "" = no match
	}{
		{"pypi.org", "package_registries"},
		{"PyPI.org", "package_registries"},  // case-insensitive
		{"pypi.org.", "package_registries"}, // trailing dot
		{"github.com", "github"},            // exact apex
		{"codeload.github.com", "github"},   // wildcard subdomain
		{"api.github.com", "github"},        // wildcard subdomain
		{"evil.com", ""},                    // not listed
		{"notgithub.com", ""},               // suffix trickery
		{"github.com.evil.com", ""},         // suffix trickery
		{"1.2.3.4", ""},                     // IP literal never matches
		{"[2001:db8::1]", ""},               // v6 literal (bracketless here) never matches
	}
	for _, c := range cases {
		got := a.MatchGroups(c.host)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("MatchGroups(%q) = %v, want none", c.host, got)
			}
			continue
		}
		if len(got) == 0 || got[0] != c.want {
			t.Errorf("MatchGroups(%q) = %v, want %q", c.host, got, c.want)
		}
	}
}

func TestWildcardDoesNotMatchApex(t *testing.T) {
	a, _ := normalize(&Allowlist{Groups: map[string][]string{"g": {"*.example.com"}}})
	if got := a.MatchGroups("example.com"); len(got) != 0 {
		t.Errorf("*.example.com must not match the apex example.com, got %v", got)
	}
	if got := a.MatchGroups("a.example.com"); len(got) == 0 {
		t.Error("*.example.com must match a.example.com")
	}
}

func TestEmptyAllowlistDeniesAll(t *testing.T) {
	a, _ := normalize(&Allowlist{})
	if got := a.MatchGroups("pypi.org"); len(got) != 0 {
		t.Errorf("empty allowlist must match nothing, got %v", got)
	}
	if !a.PortAllowed(443) {
		t.Error("default allowed port should include 443")
	}
	if a.PortAllowed(22) {
		t.Error("port 22 must not be allowed by default")
	}
}
