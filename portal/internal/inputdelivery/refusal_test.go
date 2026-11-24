package inputdelivery

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
)

func negativeProof(t *testing.T, err error, a Admission) {
	t.Helper()
	var disposition *backend.DeliveryError
	if !errors.As(err, &disposition) || disposition.NotQueued == nil || disposition.NotQueued.Version != 1 || disposition.NotQueued.Scope != a.Scope || !SameRequest(disposition.NotQueued.Request, a.Request) {
		t.Fatalf("no exact durable proof: %v", err)
	}
}

func TestAdmitRejectShareOneImmutableIdentity(t *testing.T) {
	ctx := context.Background()
	for _, first := range []string{"admit", "reject", "race"} {
		t.Run(first, func(t *testing.T) {
			root, workspace := t.TempDir(), t.TempDir()
			s, e := Open(ctx, root, workspace, testGate)
			if e != nil {
				t.Fatal(e)
			}
			a := fixtureAdmission(workspace, "1"+"1111111111111111111111111111111")
			if first == "admit" {
				if _, e = s.Admit(ctx, a); e != nil {
					t.Fatal(e)
				}
			}
			if first == "reject" {
				_, e = s.Reject(ctx, a.Scope, a.Ref.Conversation, a.Request, "delivery_selection_changed")
				negativeProof(t, e, a)
			}
			peer, e := Open(ctx, root, workspace, testGate)
			if e != nil {
				t.Fatal(e)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 16)
			for i := range 16 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					var e error
					if i%2 == 0 {
						_, e = s.Admit(ctx, a)
					} else {
						_, e = peer.Reject(ctx, a.Scope, a.Ref.Conversation, a.Request, "delivery_selection_changed")
					}
					errs <- e
				}(i)
			}
			wg.Wait()
			close(errs)
			r, resolved := s.Resolve(ctx, a.Scope, a.Request)
			admitted := resolved == nil
			if first == "admit" && !admitted || first == "reject" && admitted {
				t.Fatal("wrong winner", first, resolved)
			}
			for err := range errs {
				if admitted {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					negativeProof(t, err, a)
				}
			}
			if admitted && r.Status.Sequence != 1 {
				t.Fatal(r)
			}
			changed := a.Request
			changed.Message = "changed"
			if _, e = s.Reject(ctx, a.Scope, a.Ref.Conversation, changed, "delivery_selection_changed"); !errors.Is(e, ErrConflict) {
				t.Fatal("changed duplicate", e)
			}
			if _, e = s.Resolve(ctx, a.Scope, changed); !errors.Is(e, ErrConflict) {
				t.Fatal("changed resolve", e)
			}
			peer.Close(ctx)
			s.Close(ctx)
			s, e = Open(ctx, root, workspace, testGate)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close(ctx)
			if !admitted {
				// Lost negative reply/restart cannot let a late original POST queue.
				_, e = s.Admit(ctx, a)
				negativeProof(t, e, a)
				if rows, e := s.List(ctx, a.Scope); e != nil || len(rows) != 0 {
					t.Fatal(rows, e)
				}
				refs, e := InspectConversations(ctx, root, workspace)
				if e != nil || len(refs) != 1 || refs[0] != a.Ref.Conversation {
					t.Fatal(refs, e)
				}
				b := fixtureAdmission(workspace, "22222222222222222222222222222222")
				if _, e = s.Admit(ctx, b); e != nil {
					t.Fatal(e)
				}
				_, e = s.Admit(ctx, a)
				negativeProof(t, e, a)
			}
		})
	}
}

func TestRefusalRequiresAuthenticScopeEvidenceAndDurableCommit(t *testing.T) {
	ctx := context.Background()
	root, workspace := t.TempDir(), t.TempDir()
	deny := false
	gate := func(ctx context.Context, run func() error) error {
		if deny {
			return errors.New("storage unavailable")
		}
		return testGate(ctx, run)
	}
	s, e := Open(ctx, root, workspace, gate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close(ctx)
	a := fixtureAdmission(workspace, "11111111111111111111111111111111")
	if _, e = s.Reject(ctx, "wrong", a.Ref.Conversation, a.Request, "delivery_selection_changed"); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	wrong := a.Ref.Conversation
	wrong.Incarnation = "malformed"
	if _, e = s.Reject(ctx, a.Scope, wrong, a.Request, "delivery_selection_changed"); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	deny = true
	_, e = s.Reject(ctx, a.Scope, a.Ref.Conversation, a.Request, "delivery_selection_changed")
	var disposition *backend.DeliveryError
	if e == nil || errors.As(e, &disposition) {
		t.Fatal("storage error gained proof", e)
	}
	deny = false
	if _, e = s.Resolve(ctx, a.Scope, a.Request); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	_, e = s.Reject(ctx, a.Scope, a.Ref.Conversation, a.Request, "delivery_selection_changed")
	negativeProof(t, e, a)
	if _, e = s.db.ExecContext(ctx, `UPDATE refusals SET scope='wrong'`); e != nil {
		t.Fatal(e)
	}
	if _, e = Inspect(ctx, root, workspace); e == nil {
		t.Fatal("malformed refusal scope retained")
	}
}
