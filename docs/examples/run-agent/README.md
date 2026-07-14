# Example: run dcr402-agent for an AI agent

Give an MCP-capable agent (for example Claude Code) the ability to spend DCR
within hard, owner-set policy. dcr402-agent is a local daemon that exposes
payment tools over MCP. The agent calls the tools; it never sees a key, and
every payment is checked against your policy file before anything is signed.

## What it shows

- A `policy.yaml` that caps spend, allowlists destinations, and sets a
  human-approval threshold.
- The two ways to attach the agent to an MCP client: stdio (an `mcp.json`
  entry) and Streamable HTTP.
- Where the keys live: a payment-scoped dcrlnd macaroon for Lightning, and
  optionally a dedicated dcrwallet account for on-chain.

## Prerequisites

- Build the daemon: `cd ../../../agent && go build ./cmd/dcr402-agent`.
- A buyer dcrlnd node and a payment-scoped macaroon:

```
lncli bakemacaroon offchain:read offchain:write invoices:read invoices:write info:read \
  --save_to payment-scoped.macaroon
```

- Optional on-chain rail: a dedicated dcrwallet account reached over mutual
  TLS. See the security chapter and `../../../examples/simnet/walletcert` for
  the client-certificate bootstrap.

## Configure the policy

Copy `policy.yaml` from this directory and edit the allowlists, caps, and
approval threshold. The agent reloads it on SIGHUP; there is no tool to
change policy from inside the agent.

## Attach to an MCP client (stdio)

Copy `mcp.json` (or the block below) into your MCP client configuration,
fixing the paths:

```
claude mcp add dcr402-agent -- /path/to/dcr402-agent
```

with `DCR402_AGENT_MCP=stdio` set in the environment. The tools become
available: `wallet_status`, `estimate`, `pay_ln_invoice`, `pay_dcr_address`,
`fetch_paid`, `get_receive_address`, `create_invoice`, `payments_list`,
`receipt_get`, `approval_status`.

## Attach over HTTP (with the dashboard)

```
export DCR402_AGENT_MCP=http
export DCR402_AGENT_HTTP_ADDR=127.0.0.1:9377
export DCR402_AGENT_HTTP_TOKEN=<a bearer token>
export DCR402_AGENT_NETWORK=simnet
export DCR402_AGENT_POLICY=./policy.yaml
export DCRLND_RPC=127.0.0.1:10010
export DCRLND_TLS_CERT=$HOME/.dcrlnd-buyer/tls.cert
export DCRLND_MACAROON=./payment-scoped.macaroon
/path/to/dcr402-agent
```

MCP is served at `/mcp` (bearer-guarded) and the web dashboard at `/` on the
same address. The dashboard shows the live payment feed, budget gauges, and
pending approvals with approve and deny buttons.

## Approvals

Payments at or above the approval threshold escalate: the tool returns
`pending_approval` immediately, and the owner approves over Bison Relay
(reply `yes <id>`, `no <id>`, or `no <id> freeze`) or from the dashboard. See
`../custom-approver/` to add a channel of your own.

## End-to-end example

`../../../examples/simnet/harness.sh agent-e2e` runs the agent against dcrlnd
nodes driven by an MCP client (policy denial with a legible trace, a Lightning
payment, an on-chain send, and an approval round trip); `agent-demo` seeds
activity and leaves the dashboard up to browse.
