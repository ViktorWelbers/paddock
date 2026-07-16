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

deny contains msg if {
	input.kind == "egress"
	count(input.groups) == 0
	msg := sprintf("host %q matches no allowed egress group", [input.host])
}

deny contains msg if {
	input.kind == "egress"
	not input.port in {80, 443}
	msg := sprintf("egress to port %d is not allowed", [input.port])
}
