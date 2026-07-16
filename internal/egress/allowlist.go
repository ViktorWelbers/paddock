// Package egress is the gateway's governed internet door: an HTTP CONNECT
// proxy that sandboxes reach through the same NetworkPolicy hole as the LLM
// gateway. Every connection is authenticated with the session token, matched
// against a domain allowlist, evaluated by the OPA engine, checked against
// DNS-rebinding, and audited (allow, deny, and bytes-on-close). TLS stays
// end-to-end: the proxy tunnels, it never terminates.
package egress

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"

	"golang.org/x/net/idna"
)

// Allowlist is the static, operator-configured half of the egress decision
// (the OPA policy is the dynamic half). A host must match a group to be
// considered at all; an empty allowlist denies everything.
type Allowlist struct {
	// Groups maps a group name (e.g. "package_registries") to its host
	// patterns. A pattern is either an exact host ("pypi.org") or a
	// "*.example.com" wildcard matching sub-domains only (not the apex).
	Groups map[string][]string `json:"groups"`
	// AllowedPorts are the CONNECT target ports permitted (default {443}).
	AllowedPorts []int `json:"allowed_ports"`
	// PlainHTTP permits absolute-URI (non-CONNECT) proxying for http://
	// targets. Off by default: every real registry is https.
	PlainHTTP bool `json:"plain_http"`
	// AllowedPrivateCIDRs punches specific private ranges through the
	// rebinding defense (e.g. a corporate registry on an internal IP).
	// Empty keeps all RFC1918/CGNAT/ULA blocked.
	AllowedPrivateCIDRs []string `json:"allowed_private_cidrs"`

	privateCIDRs []netip.Prefix
}

// Load reads an allowlist JSON file. A missing path yields an empty
// (deny-all) allowlist rather than an error, so the proxy still runs and
// audits every denial when no config is mounted.
func Load(path string) (*Allowlist, error) {
	if path == "" {
		return normalize(&Allowlist{})
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return normalize(&Allowlist{})
		}
		return nil, err
	}
	var a Allowlist
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("parse egress allowlist %q: %w", path, err)
	}
	return normalize(&a)
}

func normalize(a *Allowlist) (*Allowlist, error) {
	if len(a.AllowedPorts) == 0 {
		a.AllowedPorts = []int{443}
	}
	for _, c := range a.AllowedPrivateCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("bad allowed_private_cidr %q: %w", c, err)
		}
		a.privateCIDRs = append(a.privateCIDRs, p)
	}
	return a, nil
}

// PortAllowed reports whether a CONNECT to this port is permitted.
func (a *Allowlist) PortAllowed(port int) bool {
	return slices.Contains(a.AllowedPorts, port)
}

// PrivateAllowed reports whether an otherwise-blocked private address is
// explicitly permitted by allowed_private_cidrs.
func (a *Allowlist) PrivateAllowed(addr netip.Addr) bool {
	for _, p := range a.privateCIDRs {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// MatchGroups returns the allowlist groups a host belongs to. Empty means
// the host is not allowed. Hosts are compared in their ASCII (punycode)
// form, lowercased, trailing dot stripped. IP literals never match — the
// allowlist is domain-based and IP targets defeat the rebinding defense.
func (a *Allowlist) MatchGroups(host string) []string {
	h := canonicalHost(host)
	if h == "" {
		return nil
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return nil // IP literal
	}
	var groups []string
	for name, patterns := range a.Groups {
		for _, pat := range patterns {
			if hostMatches(h, canonicalHost(pat)) {
				groups = append(groups, name)
				break
			}
		}
	}
	slices.Sort(groups)
	return groups
}

func canonicalHost(host string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return ""
	}
	if ascii, err := idna.Lookup.ToASCII(h); err == nil {
		return ascii
	}
	return h
}

// hostMatches compares an already-canonical host against a canonical
// pattern. "*.example.com" matches any sub-domain but not the apex.
func hostMatches(host, pattern string) bool {
	if pattern == "" {
		return false
	}
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix) && host != suffix
	}
	return host == pattern
}
