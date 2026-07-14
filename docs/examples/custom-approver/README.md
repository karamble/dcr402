# Example: a custom approval channel

dcr402-agent escalates over-threshold payments to a human. Bison Relay and
the web dashboard are built in, but the channel is an interface, so you can
add your own: a Slack relay, a pager, an email gateway, anything that can
show the owner a request and let them reply.

## What it shows

- Implementing `approve.Approver` (two methods: `Name` and `Notify`).
- The fail-closed contract: `Notify` returning an error marks the channel
  unreachable; if no channel is reachable, the escalation is denied.
- The notify/resolve split: `Notify` delivers the request; the owner's
  verdict comes back separately through `Registry.Resolve`, called by the
  agent web API or by a handler you write.
- Wiring it onto a running daemon with `d.Registry.AddApprover(...)`.

## The approver

`WebhookApprover` POSTs each approval request as JSON to a URL you control:

```json
{ "id": "3fa2c1", "summary": "pay 0.35 DCR to sat.example.com ...", "expires_at": "..." }
```

Your endpoint shows this to the owner. When the owner decides, your handler
calls `registry.Resolve(ctx, id, approved, responder)` (or the agent's
`POST /api/approvals/{id}` if you run the web API). On approval the parked
payment executes asynchronously, re-checked against policy.

## Run

This example is self-contained and needs no nodes. It builds a registry with
just the webhook approver, fires one escalation, and resolves it:

```
go run .
# [webhook] would POST approval a1b2c3: pay 0.35 DCR to sat.example.com ...
# escalated as approval a1b2c3; the owner approves out of band
# continuation: approved=true by owner
# approved; the payment would now execute
```

Point it at a real endpoint to see the POST:

```
WEBHOOK_URL=https://your-relay.example/approvals go run .
```

## Wiring into the agent

Name the approver for the policy channel it serves. `approval.channel` in
`policy.yaml` selects approvers by name (`bisonrelay`, `web`, or `both`), so
naming this one `bisonrelay` and leaving the real Bison Relay channel
unconfigured routes escalations to your webhook. Then:

```go
d, _ := agent.New(ctx, cfg)
d.Registry.AddApprover(&WebhookApprover{URL: "https://your-relay.example/approvals", HTTP: http.DefaultClient})
go d.Run(ctx)
```

## Related examples

The built-in channels are exercised by
`../../../examples/simnet/harness.sh agent-e2e` (web approval round trip) and
the Bison Relay approver's own test suite in
`../../../agent/approve/bisonrelay/`.
