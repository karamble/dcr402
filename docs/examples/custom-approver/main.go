// custom-approver shows how to add an approval channel of your own to
// dcr402-agent by implementing the approve.Approver interface. This one
// POSTs each approval request to a webhook (your chat relay, pager, or
// dashboard). It runs standalone, with no nodes: it exercises the same
// approve.Registry seam the daemon uses. See README.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/karamble/dcr402/agent/approve"
	"github.com/karamble/dcr402/agent/store"
)

// WebhookApprover is a custom approval channel. Notify delivers the request;
// resolution (the owner's yes or no) flows back separately through
// Registry.Resolve, called by the agent web API or by your own handler.
type WebhookApprover struct {
	URL  string
	HTTP *http.Client
}

// Name reports which policy channel this approver serves. The policy
// approval.channel selects approvers by name (bisonrelay, web, or both), so
// naming it "bisonrelay" and leaving the real Bison Relay channel
// unconfigured routes escalations here instead.
func (WebhookApprover) Name() string { return "bisonrelay" }

// Notify delivers one approval request. Returning an error marks the channel
// unreachable for this request; if no channel is reachable the escalation
// fails closed (denied).
func (a WebhookApprover) Notify(ctx context.Context, req approve.Request) error {
	body, _ := json.Marshal(map[string]any{
		"id":         req.ID,
		"summary":    req.Summary,
		"expires_at": req.ExpiresAt,
	})
	if a.URL == "" {
		log.Printf("[webhook] would POST approval %s: %s", req.ID, req.Summary)
		return nil
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func main() {
	// In a real deployment you register the approver on the running daemon:
	//
	//   d, _ := agent.New(ctx, cfg)
	//   d.Registry.AddApprover(&WebhookApprover{URL: "https://...", HTTP: http.DefaultClient})
	//   go d.Run(ctx)
	//
	// Here we drive the same approve.Registry standalone, with no nodes.
	reg := approve.NewRegistry(store.NewMemory(), time.Now,
		&WebhookApprover{URL: os.Getenv("WEBHOOK_URL"), HTTP: http.DefaultClient})

	ctx := context.Background()
	done := make(chan bool, 1)

	appr, err := reg.Escalate(ctx, 0, "bisonrelay", 2*time.Minute,
		"pay 0.35 DCR to sat.example.com (memo: monthly report)",
		func(runCtx context.Context, approved bool, responder string) {
			log.Printf("continuation: approved=%v by %s", approved, responder)
			done <- approved
		})
	if err != nil {
		// Reached only if every channel was unreachable (fail closed).
		log.Fatalf("escalation denied fail-closed: %v", err)
	}
	log.Printf("escalated as approval %s; the owner approves out of band", appr.ID)

	// The owner clicks approve. In production this Resolve call is made by
	// your web handler or webhook callback; here we make it directly.
	if _, err := reg.Resolve(ctx, appr.ID, true, "owner"); err != nil {
		log.Fatal(err)
	}
	<-done
	log.Print("approved; the payment would now execute")
}
