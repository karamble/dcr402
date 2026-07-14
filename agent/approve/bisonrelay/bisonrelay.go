// Package bisonrelay implements the approve.Approver channel over Bison
// Relay's generic clientrpc protocol: approval requests go out as PMs to
// the owner, verdicts come back as strict "yes <id>" / "no <id>" replies
// (with "freeze"/"block" as an emergency deny), consumed from the
// cumulative-ack PMStream with persistent resume. It works against any
// brclient-compatible clientrpc endpoint — no bespoke daemon required.
//
// Note the protocol's constraint: a brclient replays PMs to ONE cumulative
// ack pointer, so run a single PMStream consumer per brclient instance.
package bisonrelay

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/companyzero/bisonrelay/clientrpc/jsonrpc"
	"github.com/companyzero/bisonrelay/clientrpc/types"
	"github.com/decred/slog"

	"github.com/karamble/dcr402/agent/approve"
	"github.com/karamble/dcr402/agent/store"
)

// ackStateKey is the store key for the cumulative PMStream ack sequence.
const ackStateKey = "br_ack"

// Config locates the clientrpc endpoint and the owner.
type Config struct {
	// RPC is the WebSocket URL, e.g. wss://127.0.0.1:7676/ws.
	RPC        string
	ServerCert string
	ClientCert string
	ClientKey  string
	User       string
	Pass       string
	// Owner is who may approve: the owner's 64-hex user id; a nick is
	// rejected.
	Owner string

	// Registry resolves verdicts; Store persists the ack pointer.
	Registry *approve.Registry
	Store    store.Store

	// Freezer is the kill-switch hook: an owner "freeze"/"block" reply halts
	// all spending, "unfreeze" resumes it. Optional; nil means those words
	// only deny the named approval.
	Freezer Freezer

	// Log is optional.
	Log slog.Logger
}

// Freezer halts and resumes all spending (the agent Service satisfies it).
type Freezer interface {
	Freeze(ctx context.Context, by, reason string) error
	Unfreeze(ctx context.Context, by string) error
}

// Approver is the Bison Relay approval channel.
type Approver struct {
	cfg      Config
	client   *jsonrpc.WSClient
	chat     types.ChatServiceClient
	version  types.VersionServiceClient
	ownerUID string // lowercase hex when Owner is a uid
	log      slog.Logger

	mu     sync.Mutex
	ackSeq uint64
}

// New builds the approver (connection starts with Run).
func New(cfg Config) (*Approver, error) {
	if cfg.RPC == "" || cfg.Owner == "" {
		return nil, fmt.Errorf("bisonrelay: RPC and Owner are required")
	}
	if cfg.Registry == nil || cfg.Store == nil {
		return nil, fmt.Errorf("bisonrelay: Registry and Store are required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Disabled
	}
	opts := []jsonrpc.ClientOption{
		jsonrpc.WithWebsocketURL(cfg.RPC),
		jsonrpc.WithClientLog(log),
	}
	if cfg.ServerCert != "" {
		opts = append(opts, jsonrpc.WithServerTLSCertPath(cfg.ServerCert))
	}
	if cfg.ClientCert != "" {
		opts = append(opts, jsonrpc.WithClientTLSCert(cfg.ClientCert, cfg.ClientKey))
	}
	if cfg.User != "" {
		opts = append(opts, jsonrpc.WithClientBasicAuth(cfg.User, cfg.Pass))
	}
	client, err := jsonrpc.NewWSClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("bisonrelay: %w", err)
	}
	a := &Approver{
		cfg:     cfg,
		client:  client,
		chat:    types.NewChatServiceClient(client),
		version: types.NewVersionServiceClient(client),
		log:     log,
	}
	// The owner MUST be identified by their 64-hex user id; a self-assigned
	// nick cannot authorize spending.
	owner := strings.TrimSpace(cfg.Owner)
	if len(owner) != 64 {
		return nil, fmt.Errorf("bisonrelay: BR_OWNER must be the owner's 64-hex user id, " +
			"not a nick (nick matching is spoofable and cannot authorize spending)")
	}
	if _, err := hex.DecodeString(owner); err != nil {
		return nil, fmt.Errorf("bisonrelay: BR_OWNER must be a 64-hex user id: %w", err)
	}
	a.ownerUID = strings.ToLower(owner)
	return a, nil
}

// Name implements approve.Approver.
func (a *Approver) Name() string { return "bisonrelay" }

// Notify implements approve.Approver: PM the owner.
func (a *Approver) Notify(ctx context.Context, req approve.Request) error {
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var resp types.PMResponse
	err := a.chat.PM(sctx, &types.PMRequest{
		User: a.cfg.Owner,
		Msg:  &types.RMPrivateMessage{Message: req.Summary},
	}, &resp)
	if err != nil {
		return fmt.Errorf("bisonrelay: notify: %w", err)
	}
	return nil
}

// Run maintains the connection and consumes owner replies until ctx ends.
// Run it as a goroutine; it returns when ctx is done.
func (a *Approver) Run(ctx context.Context) error {
	go func() {
		// The WS client reconnects internally.
		_ = a.client.Run(ctx)
	}()

	// Fail-fast handshake (best effort, bounded).
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var v types.VersionResponse
	if err := a.version.Version(hctx, &types.VersionRequest{}, &v); err == nil {
		a.log.Infof("bisonrelay: connected to %s %s", v.AppName, v.AppVersion)
	}
	cancel()

	a.loadAck(ctx)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := a.consume(ctx); err != nil && ctx.Err() == nil {
			a.log.Warnf("bisonrelay: pm stream: %v (resuming)", err)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (a *Approver) loadAck(ctx context.Context) {
	if v, err := a.cfg.Store.GetState(ctx, ackStateKey); err == nil {
		if seq, err := strconv.ParseUint(v, 10, 64); err == nil {
			a.mu.Lock()
			a.ackSeq = seq
			a.mu.Unlock()
		}
	}
}

func (a *Approver) consume(ctx context.Context) error {
	a.mu.Lock()
	from := a.ackSeq
	a.mu.Unlock()
	stream, err := a.chat.PMStream(ctx, &types.PMStreamRequest{UnackedFrom: from})
	if err != nil {
		return err
	}
	for {
		var pm types.ReceivedPM
		if err := stream.Recv(&pm); err != nil {
			return err
		}
		if a.isOwner(&pm) && pm.Msg != nil {
			if err := a.handleReply(ctx, pm.Msg.Message); err != nil {
				// A durable resolve/freeze failed: do NOT advance the ack, so
				// this PM replays on reconnect and the verdict is retried.
				// Replay is idempotent - an already-applied verdict resolves
				// cleanly (won=false, no error) and then advances the ack.
				return err
			}
		}
		// Cumulative ack — advance past every message (we are the sole
		// PMStream consumer) and persist for resume.
		var ackResp types.AckResponse
		if err := a.chat.AckReceivedPM(ctx, &types.AckRequest{SequenceId: pm.SequenceId}, &ackResp); err != nil {
			return err
		}
		a.mu.Lock()
		a.ackSeq = pm.SequenceId
		a.mu.Unlock()
		_ = a.cfg.Store.SetState(ctx, ackStateKey, strconv.FormatUint(pm.SequenceId, 10))
	}
}

// isOwner matches strictly on the cryptographic user id, never the nick.
func (a *Approver) isOwner(pm *types.ReceivedPM) bool {
	return strings.EqualFold(hex.EncodeToString(pm.Uid), a.ownerUID)
}

// handleReply parses the owner's reply:
//   - "freeze" / "block" [id...]  -> the kill-switch: halt ALL spending (and
//     deny any named approval).
//   - "unfreeze" / "resume"       -> resume spending.
//   - "yes <id>" / "no <id>"      -> resolve the named approval (also
//     approve/deny/ok/y/n, case-insensitive). A trailing "freeze"/"block"
//     still trips the kill-switch and forces a deny.
//
// A bare "yes"/"no" (no id) is ignored - it cannot name a request.
//
// It returns a non-nil error ONLY when a durable action failed (a Freezer or
// Registry store write), so the caller can hold the ack and replay the PM. A
// reply that names no live approval, or is not a verdict at all, returns nil
// (the ack advances - there is nothing to retry).
func (a *Approver) handleReply(ctx context.Context, text string) error {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) == 0 {
		return nil
	}

	switch fields[0] {
	case "freeze", "block":
		if err := a.freeze(ctx); err != nil {
			return err
		}
		return a.resolveIDs(ctx, fields[1:], false, a.cfg.Owner+" (freeze)")
	case "unfreeze", "resume", "unblock":
		if a.cfg.Freezer != nil {
			return a.cfg.Freezer.Unfreeze(ctx, a.cfg.Owner)
		}
		return nil
	}

	if len(fields) < 2 {
		return nil
	}
	verdict, ok := parseVerdict(fields[0])
	if !ok {
		return nil
	}
	freeze := false
	for _, f := range fields[1:] {
		if f == "freeze" || f == "block" {
			freeze = true
			verdict = false
		}
	}
	responder := a.cfg.Owner
	if freeze {
		responder += " (freeze)"
		if err := a.freeze(ctx); err != nil {
			return err
		}
	}
	return a.resolveIDs(ctx, fields[1:], verdict, responder)
}

// freeze trips the kill-switch when a Freezer is wired.
func (a *Approver) freeze(ctx context.Context) error {
	if a.cfg.Freezer != nil {
		return a.cfg.Freezer.Freeze(ctx, a.cfg.Owner, "owner freeze over Bison Relay")
	}
	return nil
}

// resolveIDs resolves each id field (skipping the freeze/block keywords) with
// the given verdict, stopping at the first that wins. A genuine store error
// from Resolve is returned so the caller can hold the ack and retry; an id that
// simply names no live approval (won=false, no error) is skipped, and a reply
// naming no live approval at all returns nil.
func (a *Approver) resolveIDs(ctx context.Context, fields []string, verdict bool, responder string) error {
	for _, f := range fields {
		if f == "" || f == "freeze" || f == "block" {
			continue
		}
		won, err := a.cfg.Registry.Resolve(ctx, f, verdict, responder)
		if err != nil {
			return err
		}
		if won {
			return nil
		}
	}
	return nil
}

func parseVerdict(s string) (bool, bool) {
	switch s {
	case "yes", "y", "approve", "approved", "ok":
		return true, true
	case "no", "n", "deny", "denied":
		return false, true
	}
	return false, false
}

var _ approve.Approver = (*Approver)(nil)
