package agent

import (
	"context"
	"encoding/json"
	"time"
)

// freezeStateKey is the store key holding the kill-switch state.
const freezeStateKey = "frozen"

// FreezeState is the persisted kill-switch. When Frozen is true, every
// spending path denies until the owner unfreezes.
type FreezeState struct {
	Frozen bool   `json:"frozen"`
	At     string `json:"at,omitempty"`     // RFC3339
	By     string `json:"by,omitempty"`     // who froze
	Reason string `json:"reason,omitempty"` // free text
}

// FrozenError is returned by every spend path while the agent is frozen. The
// MCP surface renders it as a machine-actionable "frozen" outcome.
type FrozenError struct{ State FreezeState }

func (e *FrozenError) Error() string {
	r := e.State.Reason
	if r == "" {
		r = "spending frozen by the owner"
	}
	return "dcr402-agent: frozen: " + r
}

// Freeze halts all spending until Unfreeze. It is triggered by the owner over
// Bison Relay ("freeze") or the authenticated dashboard - never by the MCP
// tool surface the driving agent can reach.
func (s *Service) Freeze(ctx context.Context, by, reason string) error {
	blob, err := json.Marshal(FreezeState{
		Frozen: true,
		At:     s.now().UTC().Format(time.RFC3339),
		By:     by,
		Reason: reason,
	})
	if err != nil {
		return err
	}
	return s.store.SetState(ctx, freezeStateKey, string(blob))
}

// Unfreeze resumes spending. Owner action only.
func (s *Service) Unfreeze(ctx context.Context, _ string) error {
	return s.store.SetState(ctx, freezeStateKey, "")
}

// FreezeStatus reports the current kill-switch state.
func (s *Service) FreezeStatus(ctx context.Context) FreezeState {
	v, err := s.store.GetState(ctx, freezeStateKey)
	if err != nil || v == "" {
		return FreezeState{}
	}
	var st FreezeState
	if json.Unmarshal([]byte(v), &st) != nil {
		return FreezeState{}
	}
	return st
}

// notFrozen returns a *FrozenError when spending is frozen. Called at the top
// of every spend path under payMu.
func (s *Service) notFrozen(ctx context.Context) error {
	if st := s.FreezeStatus(ctx); st.Frozen {
		return &FrozenError{State: st}
	}
	return nil
}
