# Examples

- [`simnet/`](simnet/) is the live-node harness: it stands up a
  full local Decred simnet (dcrd, a voting dcrwallet, two dcrlnd nodes, an
  agent dcrwallet) and drives every component end-to-end with real payments.
  See [`simnet/README.md`](simnet/README.md) for the commands (`start`,
  `e2e`, `gateway-e2e`, `agent-e2e`, `agent-demo`).

For focused, copy-pasteable integration walkthroughs (gate an API, front a
service with dcr402d, consume paid resources, run the agent), see
[`../docs/`](../docs/). Those examples are minimal distillations of the
harness code here.
