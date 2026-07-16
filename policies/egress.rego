# Governed egress: every CONNECT through the gateway's egress proxy is
# evaluated here after the static allowlist matched. input:
# {kind: "egress", user, session, agent, host, port, groups} where groups
# are the allowlist groups the host matched (e.g. ["package_registries"]).
#
# Extension points platform teams typically add:
#   - per-user or per-agent group restrictions (input.user, input.agent)
#   - time-of-day rules, host pinning per team, ...
package paddock.authz

import rego.v1

# The proxy already refuses a host no group claims before it ever asks a
# policy, so this is the second line of defense. It has to test for the
# *absence* of groups rather than count(input.groups) == 0: `groups` is
# omitted from the input document entirely when empty, which would leave the
# count undefined and this rule quietly dead.
deny contains msg if {
	input.kind == "egress"
	not has_groups
	msg := sprintf("host %q matches no allowed egress group", [input.host])
}

has_groups if count(input.groups) > 0

deny contains msg if {
	input.kind == "egress"
	not input.port in {80, 443}
	msg := sprintf("egress to port %d is not allowed", [input.port])
}
