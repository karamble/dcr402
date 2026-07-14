package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func implementations(t *testing.T) map[string]Store {
	t.Helper()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { sq.Close() })
	return map[string]Store{"memory": NewMemory(), "sqlite": sq}
}

func TestAttemptAndPaymentRoundTrip(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0).UTC()

			aid, err := s.RecordAttempt(ctx, Attempt{
				Tool: "fetch_paid", Rail: "ln", Destination: "sat.example.com",
				AmountAtoms: 250000, Memo: "test", Decision: DecisionAllow,
				RuleTrace: []byte(`[{"rule":"cap","pass":true}]`), CreatedAt: now,
			})
			if err != nil || aid == 0 {
				t.Fatalf("RecordAttempt: id=%d err=%v", aid, err)
			}
			got, err := s.GetAttempt(ctx, aid)
			if err != nil || got.Tool != "fetch_paid" || got.Decision != DecisionAllow ||
				string(got.RuleTrace) == "" || !got.CreatedAt.Equal(now) {
				t.Fatalf("GetAttempt: %+v err=%v", got, err)
			}

			pid, err := s.RecordPayment(ctx, Payment{
				AttemptID: aid, Rail: "ln", AmountAtoms: 250000,
				Invoice: "lnsdcr...", Preimage: "11aa", L402Service: "sat.example.com",
				Receipt: []byte(`{"amount":"250000"}`), CreatedAt: now,
			})
			if err != nil || pid == 0 {
				t.Fatalf("RecordPayment: %v", err)
			}
			p, err := s.GetPayment(ctx, pid)
			if err != nil || p.AttemptID != aid || p.Preimage != "11aa" {
				t.Fatalf("GetPayment: %+v err=%v", p, err)
			}

			// Newest-first listing with pagination.
			aid2, _ := s.RecordAttempt(ctx, Attempt{
				Tool: "pay_ln_invoice", Decision: DecisionDeny,
				RuleTrace: []byte(`[]`), CreatedAt: now.Add(time.Minute),
			})
			list, err := s.ListAttempts(ctx, 10, 0)
			if err != nil || len(list) != 2 || list[0].ID != aid2 {
				t.Fatalf("ListAttempts: %+v err=%v", list, err)
			}
			list, err = s.ListAttempts(ctx, 10, aid2)
			if err != nil || len(list) != 1 || list[0].ID != aid {
				t.Fatalf("ListAttempts before: %+v err=%v", list, err)
			}
			if _, err := s.GetAttempt(ctx, 999); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing attempt: %v", err)
			}
		})
	}
}

func TestUsageWindows(t *testing.T) {
	// now = Wednesday 2026-07-08 12:00 UTC.
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if DayStart(now) != time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC) {
		t.Fatal("DayStart wrong")
	}
	if WeekStart(now) != time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC) { // Monday
		t.Fatal("WeekStart wrong")
	}

	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pay := func(atoms int64, at time.Time) {
				if _, err := s.RecordPayment(ctx, Payment{
					AttemptID: 1, Rail: "ln", AmountAtoms: atoms, CreatedAt: at,
				}); err != nil {
					t.Fatal(err)
				}
			}
			pay(100, now.Add(-30*time.Minute)) // hour + day + week
			pay(200, now.Add(-3*time.Hour))    // day + week
			pay(400, now.Add(-2*24*time.Hour)) // Monday: week only
			pay(800, now.Add(-4*24*time.Hour)) // last week: none
			u, err := s.Usage(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if u.DayAtoms != 300 || u.WeekAtoms != 700 || u.HourCount != 1 {
				t.Fatalf("usage: %+v", u)
			}
		})
	}
}

func TestTokenCache(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0).UTC()
			tok := Token{ServiceHost: "sat.example.com", Authorization: "L402 abc:11",
				ValidUntil: 1893456000, PaidAtoms: 250000, CreatedAt: now, LastUsed: now}
			if err := s.PutToken(ctx, tok); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetToken(ctx, "sat.example.com")
			if err != nil || got.Authorization != "L402 abc:11" {
				t.Fatalf("GetToken: %+v err=%v", got, err)
			}
			// Upsert replaces.
			tok.Authorization = "L402 def:22"
			if err := s.PutToken(ctx, tok); err != nil {
				t.Fatal(err)
			}
			if got, _ = s.GetToken(ctx, "sat.example.com"); got.Authorization != "L402 def:22" {
				t.Fatalf("upsert failed: %+v", got)
			}
			if err := s.TouchToken(ctx, "sat.example.com", now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if got, _ = s.GetToken(ctx, "sat.example.com"); !got.LastUsed.After(now) {
				t.Fatalf("touch failed: %+v", got)
			}
			if err := s.DeleteToken(ctx, "sat.example.com"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetToken(ctx, "sat.example.com"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("delete failed: %v", err)
			}
		})
	}
}

func TestApprovalLifecycle(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0).UTC()
			a := Approval{ID: "a1b2", AttemptID: 7, Channel: "bisonrelay",
				Status: ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
			if err := s.CreateApproval(ctx, a); err != nil {
				t.Fatal(err)
			}
			pending, err := s.ListPendingApprovals(ctx, now)
			if err != nil || len(pending) != 1 || pending[0].ID != "a1b2" {
				t.Fatalf("pending: %+v err=%v", pending, err)
			}

			// Exactly one resolver wins.
			ok, err := s.ResolveApproval(ctx, "a1b2", ApprovalApproved, "frank", now.Add(time.Minute))
			if err != nil || !ok {
				t.Fatalf("resolve: ok=%v err=%v", ok, err)
			}
			ok, err = s.ResolveApproval(ctx, "a1b2", ApprovalDenied, "other", now.Add(time.Minute))
			if err != nil || ok {
				t.Fatalf("second resolve won: ok=%v err=%v", ok, err)
			}
			got, _ := s.GetApproval(ctx, "a1b2")
			if got.Status != ApprovalApproved || got.Responder != "frank" || got.ResolvedAt.IsZero() {
				t.Fatalf("resolved state: %+v", got)
			}

			// Expiry: pending past TTL flips to expired and can't resolve.
			b := Approval{ID: "c3d4", AttemptID: 8, Channel: "web",
				Status: ApprovalPending, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
			if err := s.CreateApproval(ctx, b); err != nil {
				t.Fatal(err)
			}
			late := now.Add(2 * time.Minute)
			if ok, _ := s.ResolveApproval(ctx, "c3d4", ApprovalApproved, "x", late); ok {
				t.Fatal("resolved an expired approval")
			}
			expired, err := s.ExpireApprovals(ctx, late)
			if err != nil || len(expired) != 1 || expired[0].ID != "c3d4" {
				t.Fatalf("expire: %+v err=%v", expired, err)
			}
			if got, _ := s.GetApproval(ctx, "c3d4"); got.Status != ApprovalExpired {
				t.Fatalf("expired state: %+v", got)
			}
		})
	}
}

func TestState(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := s.GetState(ctx, "br_ack"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing state: %v", err)
			}
			if err := s.SetState(ctx, "br_ack", "42"); err != nil {
				t.Fatal(err)
			}
			if err := s.SetState(ctx, "br_ack", "43"); err != nil {
				t.Fatal(err)
			}
			if v, err := s.GetState(ctx, "br_ack"); err != nil || v != "43" {
				t.Fatalf("state: %q err=%v", v, err)
			}
		})
	}
}
