# dcr402 documentation

Technical reference and integration examples for the dcr402 suite: Decred
payments for HTTP APIs and MCP servers, built on the x402 protocol and L402
payment mechanics.

If you are new to the suite, read `architecture.md` first, then the example
that matches what you are building.

## Reference

- [architecture.md](architecture.md): components, the system map, the three
  trust topologies, and the end-to-end payment flows.
- [security.md](security.md): key isolation on both sides, replay protection,
  pay-first risk, custody exposure, the prompt-injection stance, and
  receipts.
- [glossary.md](glossary.md): the terms used across the suite.

The formal wire specification lives in `../scheme/`.

## Integration examples

Each example is a short walkthrough plus a minimal, runnable program or
config, distilled from the live-node code in `../examples/simnet/`. The Go
examples build as one module (`go build ./...` in this directory) and run
when pointed at a dcrlnd or dcrwallet (or the simnet harness).

Sell (charge for your service):

- [examples/gate-http-api](examples/gate-http-api): gate a `net/http`
  endpoint with one line, Lightning per call.
- [examples/gate-mcp-server](examples/gate-mcp-server): make one MCP tool
  payable with the go-sdk and the x402 MCP transport.
- [examples/credit-accounts](examples/credit-accounts): prepaid credit
  accounts with Lightning and on-chain top-ups.
- [examples/gateway-proxy](examples/gateway-proxy): charge for a service you
  already run by fronting it with dcr402d, from YAML, with no code.

Consume (pay for a service):

- [examples/pay-client-go](examples/pay-client-go): pay a 402 from Go with
  the dual-envelope pay-client and a credential cache.
- [examples/pay-by-hand](examples/pay-by-hand): satisfy a 402 with curl and
  your own Lightning node, from any language.

Agent (spend within policy):

- [examples/run-agent](examples/run-agent): run dcr402-agent so an MCP client
  can spend DCR within owner-set policy, with human approval.
- [examples/custom-approver](examples/custom-approver): add an approval
  channel of your own by implementing the approve.Approver interface.

Facilitate (verify and index for others):

- [examples/run-dcrbazaar](examples/run-dcrbazaar): run dcrbazaar so agents
  can verify DCR payments and discover DCR-payable services through the
  standard x402 facilitator API.

## Running the examples against nodes

The Go examples read their dcrlnd or dcrwallet connection from environment
variables, the same names the daemons use (`DCRLND_RPC`, `DCRLND_TLS_CERT`,
`DCRLND_MACAROON`, and so on). The quickest way to get a full set of live
nodes is the simnet harness:

```
../examples/simnet/harness.sh start
```

It writes the connection details to `~/.dcr402-simnet/env.sh`. See
`../examples/simnet/README.md` for the harness commands and ports.
