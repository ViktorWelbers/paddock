# Tests for the shipped egress policy. Run them with:
#
#     make policy-test          (or: opa test policies/ -v)
#
# Worth copying when you write your own rules. Rego fails open in a way that
# looks identical to working code: a rule whose body references a field that
# isn't in the input is simply undefined, so it never fires, denies nothing,
# and reads perfectly well in review. `test_ungrouped_host_is_denied` below is
# exactly that bug, caught.
package paddock.authz_test

import data.paddock.authz
import rego.v1

# The gateway omits empty fields, so a host in no allowlist group arrives with
# `groups` absent -- not as an empty list. A rule written against
# count(input.groups) == 0 is undefined here and allows everything.
test_ungrouped_host_is_denied if {
	not authz.allow with input as {
		"kind": "egress",
		"user": "viktor",
		"session": "s1",
		"host": "evil.example.com",
		"port": 443,
	}
}

# The same host, but the field is present and empty.
test_empty_group_list_is_denied if {
	not authz.allow with input as {
		"kind": "egress",
		"user": "viktor",
		"session": "s1",
		"host": "evil.example.com",
		"port": 443,
		"groups": [],
	}
}

test_allowlisted_registry_is_allowed if {
	authz.allow with input as {
		"kind": "egress",
		"user": "viktor",
		"session": "s1",
		"host": "pypi.org",
		"port": 443,
		"groups": ["package_registries"],
	}
}

test_plain_http_is_allowed_when_grouped if {
	authz.allow with input as {
		"kind": "egress",
		"user": "viktor",
		"session": "s1",
		"host": "registry.internal",
		"port": 80,
		"groups": ["internal"],
	}
}

# 443 and 80 are the web; anything else is someone tunnelling.
test_ssh_port_is_denied if {
	not authz.allow with input as {
		"kind": "egress",
		"user": "viktor",
		"session": "s1",
		"host": "github.com",
		"port": 22,
		"groups": ["github"],
	}
}

test_denial_explains_itself if {
	reasons := authz.deny with input as {
		"kind": "egress",
		"user": "viktor",
		"session": "s1",
		"host": "evil.example.com",
		"port": 443,
	}
	count(reasons) == 1
	contains(reasons[_], "evil.example.com")
}

# Egress rules must not leak into the other decision kinds.
test_egress_rules_ignore_tool_calls if {
	authz.allow with input as {
		"kind": "tool_call",
		"user": "viktor",
		"session": "s1",
		"tool": "grep",
	}
}
