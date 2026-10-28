package communication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

type testTransport struct {
	held             string
	sends            int
	lost             bool
	receipts         map[string]codex.SendReceipt
	entered, release chan struct{}
}

func (f *testTransport) Ready(context.Context, Identity) (string, error) { return f.held, nil }
func (f *testTransport) Prepare(context.Context, Identity) (codex.TurnOptions, error) {
	return codex.TurnOptions{Model: "retained-model", ReasoningEffort: "xhigh"}, nil
}
func (f *testTransport) Send(_ context.Context, m Message) (codex.SendReceipt, error) {
	f.sends++
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	r := codex.SendReceipt{ClientUserMessageID: m.ID, TurnID: "turn-" + m.ID}
	f.receipts[m.ID] = r
	if f.lost {
		return codex.SendReceipt{}, errors.New("lost acknowledgement")
	}
	return r, nil
}
func (f *testTransport) Reconcile(_ context.Context, m Message) (codex.SendReceipt, bool, error) {
	r, ok := f.receipts[m.ID]
	return r, ok, nil
}

func fixture(t *testing.T) (Service, map[string]Identity, *testTransport) {
	t.Helper()
	store, err := NewStore(t.TempDir(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]Identity{}
	for _, slug := range []string{"a", "b", "c"} {
		identities[slug] = Identity{slug, "thread-" + slug, "incarnation-" + slug}
	}
	f := &testTransport{receipts: map[string]codex.SendReceipt{}}
	return Service{Store: store, Resolve: func(slug string) (Identity, error) {
		v, ok := identities[slug]
		if !ok {
			return Identity{}, errors.New("session missing")
		}
		return v, nil
	}, Lock: func(context.Context, string) (func(), error) { return func() {}, nil }, Transport: f}, identities, f
}
func allow(t *testing.T, s Service, a Identity, peer string) {
	t.Helper()
	if _, err := s.Run(context.Background(), a, Request{Action: "allow", Peer: peer, Approval: "User explicitly requested this pair in turn user-1"}); err != nil {
		t.Fatal(err)
	}
}
func send(t *testing.T, s Service, a Identity, peer string, n int) string {
	t.Helper()
	id := fmt.Sprintf("%032x", n)
	if _, err := s.Run(context.Background(), a, Request{Action: "send", Peer: peer, MessageID: id, Message: "Question"}); err != nil {
		t.Fatal(err)
	}
	return id
}
func read(t *testing.T, s Service, a Identity) View {
	t.Helper()
	v, err := s.Read(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPairApprovalAndCallerIsolation(t *testing.T) {
	s, ids, _ := fixture(t)
	ctx := context.Background()
	if _, err := s.Run(ctx, ids["a"], Request{Action: "send", Peer: "b", MessageID: fmt.Sprintf("%032x", 1), Message: "unapproved"}); err == nil {
		t.Fatal("unapproved send accepted")
	}
	if _, err := s.Run(ctx, ids["a"], Request{Action: "allow", Peer: "b"}); err == nil {
		t.Fatal("missing approval accepted")
	}
	allow(t, s, ids["a"], "b")
	allow(t, s, ids["a"], "b")
	if len(read(t, s, ids["b"]).Grants) != 1 {
		t.Fatal("grant was not symmetric and idempotent")
	}
	send(t, s, ids["a"], "b", 1)
	send(t, s, ids["b"], "a", 2)
	if len(read(t, s, ids["c"]).Messages) != 0 || len(read(t, s, ids["c"]).Grants) != 0 {
		t.Fatal("unrelated session can read exchange")
	}
	forged := ids["a"]
	forged.Thread = "thread-c"
	if _, err := s.Run(ctx, forged, Request{Action: "allow", Peer: "b", Approval: "user"}); err == nil {
		t.Fatal("forged caller accepted")
	}
	if _, err := s.Run(ctx, ids["a"], Request{Action: "send", Peer: "c", MessageID: fmt.Sprintf("%032x", 3), Message: "not granted"}); err == nil {
		t.Fatal("grant applied to unrelated session")
	}
}

func TestHoldResumeRetryAndReply(t *testing.T) {
	s, ids, f := fixture(t)
	ctx := context.Background()
	allow(t, s, ids["a"], "b")
	id := send(t, s, ids["a"], "b", 1)
	f.held = "Recipient is stopped"
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f.sends != 0 || read(t, s, ids["a"]).Messages[0].Status != "held" {
		t.Fatal("held recipient activated")
	}
	f.held = ""
	f.lost = true
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if read(t, s, ids["a"]).Messages[0].Status != "submitting" {
		t.Fatal("lost acknowledgement not preserved")
	}
	// Re-open the store as a restarted worker, then reconcile the exact ID.
	restarted := &Store{directory: s.Store.directory, workspace: s.Store.workspace}
	s.Store = restarted
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f.sends != 1 || read(t, s, ids["a"]).Messages[0].Status != "delivered" {
		t.Fatal("retry duplicated or lost delivered message")
	}
	if send(t, s, ids["a"], "b", 1) != id || len(read(t, s, ids["a"]).Messages) != 1 {
		t.Fatal("identical retry duplicated message")
	}
	if _, err := s.Run(ctx, ids["a"], Request{Action: "send", Peer: "b", MessageID: id, Message: "changed"}); err == nil {
		t.Fatal("changed retry accepted")
	}
	if _, err := s.Run(ctx, ids["b"], Request{Action: "reply", Peer: "a", MessageID: fmt.Sprintf("%032x", 2), Message: "Answer", ReplyTo: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, ids["b"], Request{Action: "reply", Peer: "a", MessageID: fmt.Sprintf("%032x", 3), Message: "Answer", ReplyTo: "unknown"}); err == nil {
		t.Fatal("foreign reply accepted")
	}
}

func TestRevocationExpiryAndReapproval(t *testing.T) {
	for _, action := range []string{"revoke", "archive", "identity"} {
		t.Run(action, func(t *testing.T) {
			s, ids, f := fixture(t)
			ctx := context.Background()
			allow(t, s, ids["a"], "b")
			id := send(t, s, ids["a"], "b", 1)
			switch action {
			case "revoke":
				if _, err := s.Run(ctx, ids["b"], Request{Action: "revoke", Peer: "a"}); err != nil {
					t.Fatal(err)
				}
			case "archive":
				if err := s.Expire(ctx, "b"); err != nil {
					t.Fatal(err)
				}
			case "identity":
				v := ids["b"]
				v.Incarnation = "replacement"
				ids["b"] = v
			}
			if err := s.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			if f.sends != 0 || read(t, s, ids["a"]).Messages[0].Status != "canceled" {
				t.Fatal("invalid approval delivered")
			}
			allow(t, s, ids["a"], "b")
			if err := s.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			if f.sends != 0 {
				t.Fatal("reapproval revived canceled traffic")
			}
		})
	}
}

func TestRevokeReconcilesAlreadySubmittedMessage(t *testing.T) {
	s, ids, f := fixture(t)
	ctx := context.Background()
	allow(t, s, ids["a"], "b")
	id := send(t, s, ids["a"], "b", 1)
	f.lost = true
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(ctx, ids["b"], Request{Action: "revoke", Peer: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f.sends != 1 || read(t, s, ids["a"]).Messages[0].Status != "delivered" {
		t.Fatal("revocation lost uncertain acceptance")
	}
}

func TestConcurrentRevocationWaitsForAdmittedSend(t *testing.T) {
	s, ids, f := fixture(t)
	ctx := context.Background()
	allow(t, s, ids["a"], "b")
	id := send(t, s, ids["a"], "b", 1)
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() { defer wg.Done(); errs <- s.Dispatch(ctx, id) }()
	<-f.entered
	go func() {
		defer wg.Done()
		_, err := s.Run(ctx, ids["b"], Request{Action: "revoke", Peer: "a"})
		errs <- err
	}()
	close(f.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.sends != 1 || read(t, s, ids["b"]).Grants[0].RevokedAt == nil {
		t.Fatal("concurrent revocation lost")
	}
}

func TestCapacityKeepsDeliveryRevocationAndExpirationWritable(t *testing.T) {
	for _, action := range []string{"revoke", "archive"} {
		t.Run(action, func(t *testing.T) {
			s, ids, transport := fixture(t)
			ctx := context.Background()
			allow(t, s, ids["a"], "b")
			id := send(t, s, ids["a"], "b", 1)
			send(t, s, ids["b"], "a", 2)
			// Model a long retained history with two accepted pending messages. Fill
			// admission to within 100 bytes while preserving its completion allowance.
			err := s.Store.locked(ctx, true, func(st *state) error {
				raw, _ := json.Marshal(st)
				target := maxStateBytes - 2*maxOptionsBytes - 2*maxReceiptBytes - 3*transitionBytes - 100
				size := len(raw)
				for number := 10; ; number++ {
					m := Message{ID: fmt.Sprintf("%032x", number), GrantID: st.Grants[0].ID, From: ids["a"], To: ids["b"], Text: "x", CreatedAt: time.Now().UTC(), Status: "canceled"}
					encoded, _ := json.Marshal(m)
					available := target - size - len(encoded)
					if available < 1 {
						break
					}
					m.Text = strings.Repeat("x", min(available, session.MaxMessageBytes))
					encoded, _ = json.Marshal(m)
					size += len(encoded) + 1
					st.Messages = append(st.Messages, m)
				}
				return s.Store.admit(st)
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Run(ctx, ids["a"], Request{Action: "send", Peer: "b", MessageID: fmt.Sprintf("%032x", 3), Message: "new"}); err == nil {
				t.Fatal("new message consumed recovery capacity")
			}
			if err = s.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			if transport.sends != 1 || read(t, s, ids["a"]).Messages[0].Status != "delivered" {
				t.Fatal("accepted delivery could not finish at capacity")
			}
			if action == "archive" {
				err = s.Expire(ctx, "b")
			} else {
				_, err = s.Run(ctx, ids["b"], Request{Action: "revoke", Peer: "a"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if read(t, s, ids["a"]).Messages[1].Status != "canceled" {
				t.Fatal("pending message was not canceled at capacity")
			}
			if read(t, s, ids["a"]).Grants[0].RevokedAt == nil {
				t.Fatal("full history prevented revocation")
			}
		})
	}
}
