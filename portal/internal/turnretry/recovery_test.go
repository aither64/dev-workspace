package turnretry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

func savedUnsubmitted(t *testing.T, c Controller, now time.Time) Pending {
	t.Helper()
	tx, err := c.Store.Lock(context.Background(), fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	tx.State.Boundary = "incarnation"
	tx.State.Enroll(Observation{})
	tx.State.AddSnapshot(proof("failed"))
	if err := tx.State.Observe(Observation{"failed", "failed", "cyberPolicy"}, now, 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.State.Begin(now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	return *tx.State.Pending
}

func TestControllerDelayedUnsubmittedCyberReservation(t *testing.T) {
	for _, delay := range []time.Duration{299 * time.Second, 301 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			c, b, now := controllerFixture(t)
			p := savedUnsubmitted(t, c, *now)
			*now = now.Add(delay)
			b.o = Observation{"failed", "failed", "cyberPolicy"}
			if err := c.ReconcileOnly(context.Background(), fixtureScope()); err != nil {
				t.Fatal(err)
			}
			if b.sends != 0 {
				t.Fatal("reconciliation dispatched")
			}
			if err := c.Step(context.Background(), fixtureScope()); err != nil {
				t.Fatal(err)
			}
			s, err := c.Store.Load(fixtureScope())
			if err != nil {
				t.Fatal(err)
			}
			if b.sends != 1 || s.Pending.ID != p.ID || s.Attempt != 1 || s.Cyber[0].AtMS != now.UnixMilli() {
				t.Fatal("stale reservation reused", s)
			}
			// Twenty-nine subsequent possible sends fit; a thirtieth must not.
			for i := 0; i < 30; i++ {
				tx, err := c.Store.Lock(context.Background(), fixtureScope())
				if err != nil {
					t.Fatal(err)
				}
				tx.State.Pending = nil
				tx.State.Phase = "idle"
				turn := fmt.Sprintf("next-%d", i)
				tx.State.AddSnapshot(proof(turn))
				if err := tx.State.Observe(Observation{turn, "failed", "cyberPolicy"}, *now, 0, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
					t.Fatal(err)
				}
				if err := tx.State.Begin(*now, Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil && i < 29 {
					t.Fatal(err)
				}
				if i == 29 && (tx.State.Pending != nil || tx.State.Phase != "exhausted") {
					t.Fatal("31st possible send admitted")
				}
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
			}
		})
	}
}

func TestControllerUnsubmittedReservationCountsOtherIDsAndClock(t *testing.T) {
	for _, others := range []int{29, 30} {
		t.Run(fmt.Sprint(others), func(t *testing.T) {
			c, b, now := controllerFixture(t)
			p := savedUnsubmitted(t, c, *now)
			*now = now.Add(301 * time.Second)
			tx, _ := c.Store.Lock(context.Background(), fixtureScope())
			for i := 0; i < others; i++ {
				id, _ := NewID()
				tx.State.Cyber = append(tx.State.Cyber, Charge{ID: id, AtMS: now.UnixMilli()})
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			b.o = Observation{"failed", "failed", "cyberPolicy"}
			if err := c.Step(context.Background(), fixtureScope()); err != nil {
				t.Fatal(err)
			}
			s, _ := c.Store.Load(fixtureScope())
			if b.sends != 30-others || s.CyberUsed(now.UnixMilli()) != 30 {
				t.Fatal("other reservations were removed", s)
			}
			if others == 29 && s.Pending.ID != p.ID {
				t.Fatal("replacement UUID")
			}
			if others == 30 && s.Phase != "exhausted" {
				t.Fatal("quota not exhausted")
			}
			if others == 30 {
				*now = now.Add(time.Hour)
				if err := c.Step(context.Background(), fixtureScope()); err != nil {
					t.Fatal(err)
				}
				if b.sends != 0 {
					t.Fatal("timer rearmed exhaustion")
				}
			}
		})
	}
	// Equal timestamps carry distinct identities; renewing one leaves the other.
	c, _, now := controllerFixture(t)
	p := savedUnsubmitted(t, c, *now)
	tx, _ := c.Store.Lock(context.Background(), fixtureScope())
	other, _ := NewID()
	tx.State.Cyber = append(tx.State.Cyber, Charge{ID: other, AtMS: p.ChargedAtMS})
	if err := tx.State.ReserveUnsubmitted(now.Add(-time.Minute), Decision{Revision: "test-policy", Capacity: true, Cybersecurity: true}); err != nil {
		t.Fatal(err)
	}
	if len(tx.State.Cyber) != 2 || tx.State.Cyber[0].ID != other || tx.State.Pending.ChargedAtMS != p.ChargedAtMS {
		t.Fatal("equal timestamp charge removed or backwards time accepted")
	}
	tx.Close()
}

func TestControllerDelayedUnknownAcceptanceReanchorsOnceWithoutResend(t *testing.T) {
	c, b, now := controllerFixture(t)
	p := savedUnsubmitted(t, c, *now)
	b.sends = 1
	b.unknown = true
	b.never = false
	*now = now.Add(301 * time.Second)
	if err := c.ReconcileOnly(context.Background(), fixtureScope()); err == nil {
		t.Fatal("unknown outcome released")
	}
	s, _ := c.Store.Load(fixtureScope())
	if s.Cyber[0].AtMS != p.ChargedAtMS || b.sends != 1 {
		t.Fatal("unknown was recharged or resent")
	}
	b.accepted = "accepted"
	b.unknown = false
	if err := c.ReconcileOnly(context.Background(), fixtureScope()); err != nil {
		t.Fatal(err)
	}
	s, _ = c.Store.Load(fixtureScope())
	at := s.Cyber[0].AtMS
	if at != now.UnixMilli() || !s.Cyber[0].Accepted || s.Pending.ID != p.ID {
		t.Fatal("delayed acceptance expired too soon")
	}
	tx, _ := c.Store.Lock(context.Background(), fixtureScope())
	tx.State.Phase = "reconciling"
	tx.State.Pending.ResultTurn = ""
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	*now = now.Add(time.Minute)
	if err := c.ReconcileOnly(context.Background(), fixtureScope()); err != nil {
		t.Fatal(err)
	}
	s, _ = c.Store.Load(fixtureScope())
	if s.Cyber[0].AtMS != at || len(s.Cyber) != 1 || b.sends != 1 {
		t.Fatal("duplicate receipt changed charge")
	}
}

func TestRetryFinalSchemaRejectsPreliminaryShapes(t *testing.T) {
	c, _, now := controllerFixture(t)
	savedUnsubmitted(t, c, *now)
	s, _ := c.Store.Load(fixtureScope())
	data, _ := json.Marshal(s)
	for name, change := range map[string]func(map[string]any){
		"missing epoch":          func(v map[string]any) { delete(v, "epoch") },
		"anonymous quota":        func(v map[string]any) { v["cyber"] = []any{float64(now.UnixMilli())} },
		"duplicate charge":       func(v map[string]any) { v["cyber"] = append(v["cyber"].([]any), v["cyber"].([]any)[0]) },
		"missing snapshot epoch": func(v map[string]any) { delete(v["snapshots"].([]any)[0].(map[string]any), "epoch") },
		"wrong pending charge":   func(v map[string]any) { v["pending"].(map[string]any)["chargedAtMs"] = float64(1) },
	} {
		t.Run(name, func(t *testing.T) {
			var raw map[string]any
			_ = json.Unmarshal(data, &raw)
			change(raw)
			bad, _ := json.Marshal(raw)
			if err := os.WriteFile(c.Store.path(fixtureScope()), bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Store.Load(fixtureScope()); err == nil {
				t.Fatal("preliminary state accepted")
			}
		})
	}
	if err := os.WriteFile(c.Store.path(fixtureScope()), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.Load(fixtureScope()); err != nil {
		t.Fatal(err)
	}
}

type delayedReceiptBackend struct {
	*fakeBackend
	now *time.Time
}

func (b delayedReceiptBackend) Continue(ctx context.Context, scope Scope, p Pending) (codex.SendReceipt, error) {
	*b.now = b.now.Add(301 * time.Second)
	return b.fakeBackend.Continue(ctx, scope, p)
}
func TestControllerAcceptedNetworkDelayReanchorsBeforeNextRetry(t *testing.T) {
	c, b, now := controllerFixture(t)
	c.Backend = delayedReceiptBackend{b, now}
	if err := c.Step(context.Background(), fixtureScope()); err != nil {
		t.Fatal(err)
	}
	b.o = Observation{"failed", "failed", "cyberPolicy"}
	if err := c.Step(context.Background(), fixtureScope()); err != nil {
		t.Fatal(err)
	}
	s, err := c.Store.Load(fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	if s.CyberUsed(now.UnixMilli()) != 1 || s.Cyber[0].AtMS != now.UnixMilli() || !s.Cyber[0].Accepted {
		t.Fatal("network delay expired a possible send", s)
	}
}

func TestRetrySettingsBarrierRetainsOldUncertaintyWithoutNewAuthority(t *testing.T) {
	c, _, now := controllerFixture(t)
	p := savedUnsubmitted(t, c, *now)
	tx, err := c.Store.Lock(context.Background(), fixtureScope())
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enabled = false
	baseline, enrolled := tx.State.Baseline, tx.State.Enrolled
	if err := tx.State.InvalidateSettings("active-manual"); err != nil {
		t.Fatal(err)
	}
	if tx.State.Epoch != 2 || tx.State.Enabled || tx.State.Enrolled != enrolled || tx.State.Baseline != baseline || !tx.State.Uncertain() || tx.State.Pending.Snapshot.Digest != p.Snapshot.Digest || tx.State.Pending.Snapshot.Epoch != 1 || len(tx.State.Cyber) != 1 {
		t.Fatal("settings boundary rewrote old authority")
	}
	if err := tx.State.Accepted("accepted-old", *now); err != nil {
		t.Fatal(err)
	}
	if tx.State.Phase != "cancelled" || tx.State.Proof("accepted-old") != nil || tx.State.Pending.Snapshot.Epoch != 1 {
		t.Fatal("old receipt acquired current authority")
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	saved, err := c.Store.Load(fixtureScope())
	if err != nil || saved.Epoch != 2 || saved.Enabled {
		t.Fatal("restart lost settings barrier", err)
	}
	saved.Epoch = ^uint64(0)
	if err := saved.InvalidateSettings("active"); err == nil {
		t.Fatal("epoch wrapped")
	}
}
