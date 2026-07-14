# Baseline authz for Paddock sandboxes. The gateway queries data.paddock.authz
# with input: {kind, user, session, tool, server, args}.
#
# Contract: `allow` must be true for the call to proceed; entries in `deny`
# become the reasons shown to the developer and written to the audit log.
package paddock.authz

import rego.v1

default allow := false

# Tools that can exfiltrate data over the network have no business running
# inside a sandbox — all sanctioned egress goes through the gateway.
network_tools := {"curl", "wget", "nc", "ssh", "scp"}

deny contains msg if {
	input.kind == "tool_call"
	input.tool in network_tools
	msg := sprintf("network tool %q is not allowed in sandboxes", [input.tool])
}

# MCP servers reach the sandbox only via the gateway's central registry;
# this rule is a second line of defense should the registry be misconfigured
# with an empty server name.
deny contains msg if {
	input.kind == "mcp_call"
	input.server == ""
	msg := "MCP call without a server name"
}

allow if count(deny) == 0
