package communication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type endpointPiTransport struct {
	mu               sync.Mutex
	t                *testing.T
	store            *Store
	phase            string
	err              error
	held             bool
	sends, lookups   int
	last             PiMessageRecord
	entered, release chan struct{}
	prepares         int
}

func (f *endpointPiTransport) Prepare(context.Context, EndpointRecord) (PiPreparation, error) {
	f.prepares++
	if f.held {
		return PiPreparation{}, piruntime.ErrHeld
	}
	return PiPreparation{Generation: strings.Repeat("a", 64), Settings: monitoroperation.PiSettingsRecord{Provider: "fixture", AccountRef: "account", Model: "model", Thinking: "low", PolicyRevision: strings.Repeat("b", 64), Revision: 1}}, nil
}
func (f *endpointPiTransport) receipt(m PiMessageRecord) piruntime.SubmissionStatus {
	input, err := expectedPiInput(m)
	if err != nil {
		f.t.Fatal(err)
	}
	d := m.PiDelivery
	result := piruntime.SubmissionStatus{Schema: 1, Ref: d.Input.Ref(), Settings: d.Settings.Runtime(), Input: input, Generation: d.Generation, Phase: f.phase}
	if f.phase == "Accepted" {
		result.Witness = &piruntime.NativeWitness{SessionID: m.To.Pi.NativeID, EntryID: "consumed-entry", EffectiveDigest: piruntime.MessageDigest(m.Envelope)}
	}
	if f.phase == "NotSubmitted" {
		result.Refusal = "explicit fixture refusal"
	}
	return result
}
func (f *endpointPiTransport) Send(_ context.Context, m PiMessageRecord) (backend.SendResult, piruntime.SubmissionStatus, error) {
	f.sends++
	f.last = m
	if f.entered != nil {
		close(f.entered)
		<-f.release
	}
	state, err := f.store.read()
	if err != nil {
		f.t.Fatal(err)
	}
	found := false
	for _, saved := range state.aggregate().Messages {
		if saved.ID == m.ID && saved.Status == "submitting" && reflect.DeepEqual(saved.PiDelivery, m.PiDelivery) && saved.Envelope == m.Envelope {
			found = true
		}
	}
	if !found {
		f.t.Fatal("native Send preceded exact durable capture")
	}
	if f.err != nil {
		return backend.SendResult{Outcome: backend.Unknown}, piruntime.SubmissionStatus{}, f.err
	}
	outcome := backend.Accepted
	if f.phase == "Unknown" {
		outcome = backend.Pending
	}
	if f.phase == "NotSubmitted" {
		outcome = backend.NotSubmitted
	}
	return backend.SendResult{Outcome: outcome}, f.receipt(m), nil
}
func (f *endpointPiTransport) Lookup(_ context.Context, m PiMessageRecord) (piruntime.SubmissionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return f.receipt(m), f.err
}

func endpointServiceFixture(t *testing.T) (Service, map[string]EndpointRecord, *testTransport, *endpointPiTransport) {
	t.Helper()
	s, legacy, native := fixture(t)
	endpoints := map[string]EndpointRecord{}
	for _, slug := range []string{"a", "b"} {
		value := legacy[slug]
		endpoints[slug] = EndpointRecord{Codex: &value}
	}
	for i, slug := range []string{"p", "q"} {
		endpoints[slug] = EndpointRecord{Pi: &PiEndpoint{Backend: "pi", Session: monitoroperation.PiSessionRecord{Workspace: "/workspace", Slug: slug, Incarnation: strings.Repeat(fmt.Sprint(i+1), 64)}, LeadIncarnation: strings.Repeat("c", 64), NativeID: "native-" + slug}}
	}
	s.ResolveEndpoint = func(slug string) (EndpointRecord, error) {
		endpoint, ok := endpoints[slug]
		if !ok {
			return EndpointRecord{}, piruntime.ErrScope
		}
		return endpoint, nil
	}
	s.AuthorizeEndpoint = func(_ context.Context, e EndpointRecord) error {
		if !e.equal(endpoints[e.slug()]) {
			return piruntime.ErrScope
		}
		return nil
	}
	s.Store.Schema2Write = func(_ context.Context, run func() error) error { return run() }
	pi := &endpointPiTransport{t: t, store: s.Store, phase: "Accepted"}
	s.Pi = pi
	return s, endpoints, native, pi
}

func TestTypedCommunicationFourDirections(t *testing.T) {
	for _, pair := range [][2]string{{"a", "b"}, {"a", "p"}, {"p", "a"}, {"p", "q"}} {
		t.Run(pair[0]+"-"+pair[1], func(t *testing.T) {
			s, ids, native, pi := endpointServiceFixture(t)
			ctx := context.Background()
			from, to := ids[pair[0]], ids[pair[1]]
			approval := "  Exact user approval and reference user-17  "
			view, err := s.RunEndpoint(ctx, from, Request{Action: "allow", Peer: to.slug(), Approval: approval})
			if err != nil || len(view.Grants) != 1 || view.Grants[0].Approval != approval {
				t.Fatal("approval/reference changed", view, err)
			}
			id := strings.Repeat("e", 32)
			request := Request{Action: "send", Peer: to.slug(), MessageID: id, Message: "quoted peer text"}
			if _, err := s.RunEndpoint(ctx, from, request); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RunEndpoint(ctx, from, request); err != nil {
				t.Fatal(err)
			}
			request.Message = "different"
			if _, err := s.RunEndpoint(ctx, from, request); err == nil {
				t.Fatal("ID reused with new text")
			}
			pending, err := s.PendingEndpoints(ctx)
			if err != nil || len(pending) != 1 {
				t.Fatal("mixed pending projection lost input", pending, err)
			}
			if err := s.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			view, err = s.ReadEndpoint(ctx, to)
			if err != nil || len(view.Messages) != 1 || view.Messages[0].Status != "delivered" || view.Messages[0].Envelope != "" || view.Messages[0].Options != nil || view.Messages[0].PiDelivery != nil || view.Messages[0].Receipt != nil {
				t.Fatal("public typed view lost record/leaked private tuple", view, err)
			}
			if to.Pi != nil && pi.sends != 1 || to.Codex != nil && native.sends != 1 {
				t.Fatal("wrong native arm", native.sends, pi.sends)
			}
			if _, err := s.RunEndpoint(ctx, to, Request{Action: "reply", Peer: from.slug(), ReplyTo: id, MessageID: strings.Repeat("f", 32), Message: "reply without duplicate approval"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Dispatch(ctx, strings.Repeat("f", 32)); err != nil {
				t.Fatal(err)
			}
			if pending, err := s.PendingEndpoints(ctx); err != nil || len(pending) != 0 {
				t.Fatal("terminal input remained scheduled", pending, err)
			}
		})
	}
}

func TestTypedPiCaptureIsLookupOnlyAfterLostReplyAndRevocation(t *testing.T) {
	s, ids, _, pi := endpointServiceFixture(t)
	ctx := context.Background()
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "allow", Peer: "p", Approval: "explicit user reference"}); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "send", Peer: "p", MessageID: id, Message: "original"}); err != nil {
		t.Fatal(err)
	}
	pi.err = errors.New("lost acknowledgement")
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "revoke", Peer: "a"}); err != nil {
		t.Fatal(err)
	}
	pi.err = nil
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Dispatch(ctx, id) }()
	}
	wg.Wait()
	view, err := s.ReadEndpoint(ctx, ids["a"])
	if err != nil || view.Messages[0].Status != "delivered" || pi.sends != 1 {
		t.Fatal("retained input was resent/erased", pi.sends, view, err)
	}
}

func TestTypedPiReceiptUsesOnlyDigestSelectedOriginalTuple(t *testing.T) {
	s, ids, _, pi := endpointServiceFixture(t)
	ctx := context.Background()
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "allow", Peer: "p", Approval: "user reference"}); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "send", Peer: "p", MessageID: id, Message: "exact peer text"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"communication:" + pi.last.GrantID, "pi-delivery-v1:steer"} {
		t.Run(action, func(t *testing.T) {
			data, _ := json.Marshal(pi.last)
			var m PiMessageRecord
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			expected := piruntime.SendInput{Message: m.Envelope, ActionContext: action}
			m.PiDelivery.Input.Digest = piruntime.SubmissionDigest(m.PiDelivery.Input.Ref(), expected, m.PiDelivery.Settings.Runtime())
			input, err := expectedPiInput(m)
			if err != nil || input != expected {
				t.Fatal(input, err)
			}
			good := pi.receipt(m)
			if piReceiptOutcome(m, input, good) != "accepted" {
				t.Fatal("valid original rejected")
			}
			for _, test := range []struct {
				name   string
				mutate func(*piruntime.SubmissionStatus)
			}{
				{"schema", func(r *piruntime.SubmissionStatus) { r.Schema = 2 }},
				{"ref", func(r *piruntime.SubmissionStatus) { r.Ref.OperationID = strings.Repeat("f", 32) }},
				{"envelope", func(r *piruntime.SubmissionStatus) { r.Input.Message += "changed" }},
				{"settings", func(r *piruntime.SubmissionStatus) { r.Settings.Model = "other" }},
				{"generation", func(r *piruntime.SubmissionStatus) { r.Generation = strings.Repeat("f", 64) }},
				{"context", func(r *piruntime.SubmissionStatus) { r.Input.ActionContext = "borrowed-context" }},
				{"followUp", func(r *piruntime.SubmissionStatus) { r.Input.ActionContext = "pi-delivery-v1:followUp" }},
				{"retry", func(r *piruntime.SubmissionStatus) { r.Input.Retry = true }},
				{"foreign-native", func(r *piruntime.SubmissionStatus) { r.Witness.SessionID = "foreign" }},
				{"missing-entry", func(r *piruntime.SubmissionStatus) { r.Witness.EntryID = "" }},
				{"foreign-digest", func(r *piruntime.SubmissionStatus) { r.Witness.EffectiveDigest = strings.Repeat("0", 64) }},
				{"missing-witness", func(r *piruntime.SubmissionStatus) { r.Witness = nil }},
			} {
				t.Run(test.name, func(t *testing.T) {
					r := pi.receipt(m)
					test.mutate(&r)
					if piReceiptOutcome(m, input, r) != "unknown" {
						t.Fatal("foreign tuple confirmed")
					}
				})
			}
			for _, phase := range []string{"Pending", "Unknown", "NotSubmitted"} {
				r := pi.receipt(m)
				r.Phase = phase
				r.Refusal = ""
				if piReceiptOutcome(m, input, r) != "unknown" {
					t.Fatal("phase inferred", phase)
				}
				if phase == "NotSubmitted" {
					r.Refusal = "explicit"
					if piReceiptOutcome(m, input, r) != "not_submitted" {
						t.Fatal("negative lost")
					}
				}
			}
			m.Envelope += "changed"
			if _, err := expectedPiInput(m); err == nil {
				t.Fatal("changed saved envelope accepted")
			}
		})
	}
}

func TestTypedMixedAdmissionExpiryAndNextCallFreshness(t *testing.T) {
	for _, bad := range []string{"missing", "skipped", "duplicate", "changed-identity", "held"} {
		t.Run(bad, func(t *testing.T) {
			s, ids, _, pi := endpointServiceFixture(t)
			ctx := context.Background()
			if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "allow", Peer: "p", Approval: "user reference"}); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("e", 32)
			if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "send", Peer: "p", MessageID: id, Message: "original"}); err != nil {
				t.Fatal(err)
			}
			switch bad {
			case "missing":
				s.Store.Schema2Write = nil
			case "skipped":
				s.Store.Schema2Write = func(context.Context, func() error) error { return nil }
			case "duplicate":
				s.Store.Schema2Write = func(_ context.Context, run func() error) error {
					if err := run(); err != nil {
						return err
					}
					return run()
				}
			case "changed-identity":
				p := *ids["p"].Pi
				p.NativeID = "replacement"
				ids["p"] = EndpointRecord{Pi: &p}
			case "held":
				pi.held = true
			}
			err := s.Dispatch(ctx, id)
			if (bad == "missing" || bad == "skipped" || bad == "duplicate") && err == nil {
				t.Fatal("invalid writer admitted")
			}
			if bad != "duplicate" && pi.sends != 0 {
				t.Fatal("native send before admission")
			}
			if bad == "held" {
				pi.held = false
				if err := s.Dispatch(ctx, id); err != nil || pi.sends != 1 {
					t.Fatal("next call reused held proof", err, pi.sends)
				}
			}
		})
	}
	s, ids, _, pi := endpointServiceFixture(t)
	ctx := context.Background()
	if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "allow", Peer: "q", Approval: "user reference"}); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "send", Peer: "q", MessageID: id, Message: "unsent"}); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingEndpoints(ctx); err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	if err := s.Expire(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingEndpoints(ctx); err != nil || len(pending) != 0 {
		t.Fatal("expiry cache stale", pending, err)
	}
	if _, err := s.RunEndpoint(ctx, ids["q"], Request{Action: "allow", Peer: "p", Approval: "new approval"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(ctx, id); err != nil || pi.sends != 0 {
		t.Fatal("reapproval revived canceled input", err)
	}
}

func TestTypedMixedCodexCapturedOptionsNeverResendOrChange(t *testing.T) {
	s, ids, native, pi := endpointServiceFixture(t)
	ctx := context.Background()
	if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "allow", Peer: "a", Approval: "user reference"}); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "send", Peer: "a", MessageID: id, Message: "original"}); err != nil {
		t.Fatal(err)
	}
	native.lost = true
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	st, err := s.Store.read()
	if err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(st.aggregate())
	for _, test := range []struct {
		name   string
		mutate func(*PiMessageRecord)
	}{
		{"text", func(m *PiMessageRecord) { m.Text += "changed" }},
		{"reply", func(m *PiMessageRecord) { m.ReplyTo = strings.Repeat("f", 32) }},
		{"grant", func(m *PiMessageRecord) { m.GrantID = strings.Repeat("f", 32) }},
		{"envelope", func(m *PiMessageRecord) { m.Envelope += "changed" }},
		{"options", func(m *PiMessageRecord) { m.Options.Model = "changed" }},
		{"created", func(m *PiMessageRecord) { m.CreatedAt = m.CreatedAt.Add(time.Second) }},
		{"cancel", func(m *PiMessageRecord) { m.Status = "canceled" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var next PiCommunicationState
			if err := json.Unmarshal(old, &next); err != nil {
				t.Fatal(err)
			}
			test.mutate(&next.Messages[0])
			if validatePiCommunicationTransition(old, next) == nil {
				t.Fatal("frozen mixed tuple changed")
			}
		})
	}
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "revoke", Peer: "p"}); err != nil {
		t.Fatal(err)
	}
	pi.held = true // Retained receipt lookup precedes new-delivery readiness.
	// Even absent native receipt cannot authorize replay; a later original witness can.
	receipt := native.receipts[id]
	delete(native.receipts, id)
	if err := s.Dispatch(ctx, id); err != nil || native.sends != 1 {
		t.Fatal("missing receipt resent", err)
	}
	native.receipts[id] = receipt
	if err := s.Dispatch(ctx, id); err != nil || native.sends != 1 {
		t.Fatal("original lookup resent", err)
	}
	view, err := s.ReadEndpoint(ctx, ids["a"])
	if err != nil || view.Messages[0].Status != "delivered" {
		t.Fatal(view, err)
	}
}

func TestTypedPiWaitRevalidatesBeforeDeliveryAndRevocationWaits(t *testing.T) {
	for _, change := range []string{"identity", "held"} {
		t.Run(change, func(t *testing.T) {
			s, ids, _, pi := endpointServiceFixture(t)
			ctx := context.Background()
			if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "allow", Peer: "p", Approval: "user reference"}); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("e", 32)
			if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "send", Peer: "p", MessageID: id, Message: "original"}); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			s.Lock = func(ctx context.Context, _ string) (func(), error) {
				once.Do(func() { close(entered); <-release })
				return func() {}, ctx.Err()
			}
			done := make(chan error, 1)
			go func() { done <- s.Dispatch(ctx, id) }()
			<-entered
			if change == "held" {
				pi.held = true
			} else {
				p := *ids["p"].Pi
				p.NativeID = "replacement"
				ids["p"] = EndpointRecord{Pi: &p}
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if pi.sends != 0 {
				t.Fatal("delivery reused pre-wait identity/readiness")
			}
		})
	}
	s, ids, _, pi := endpointServiceFixture(t)
	ctx := context.Background()
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "allow", Peer: "p", Approval: "user reference"}); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 32)
	if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "send", Peer: "p", MessageID: id, Message: "original"}); err != nil {
		t.Fatal(err)
	}
	pi.entered, pi.release = make(chan struct{}), make(chan struct{})
	sent := make(chan error, 1)
	go func() { sent <- s.Dispatch(ctx, id) }()
	<-pi.entered
	revoked := make(chan error, 1)
	go func() { _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "revoke", Peer: "p"}); revoked <- err }()
	select {
	case err := <-revoked:
		t.Fatal("revocation crossed admitted capture/send", err)
	default:
	}
	close(pi.release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(ctx, id); err != nil || pi.sends != 1 {
		t.Fatal("accepted Pi message replayed after revoke", err)
	}
}

func TestTypedRevokeKeepsAdmissionWithoutDeliveryReadiness(t *testing.T) {
	for _, side := range []string{"source", "recipient"} {
		t.Run(side, func(t *testing.T) {
			s, ids, _, pi := endpointServiceFixture(t)
			ctx := context.Background()
			from, to := ids["a"], ids["p"]
			if side == "source" {
				from, to = to, from
			}
			if _, err := s.RunEndpoint(ctx, from, Request{Action: "allow", Peer: to.Slug(), Approval: "exact user reference"}); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("e", 32)
			if _, err := s.RunEndpoint(ctx, from, Request{Action: "send", Peer: to.Slug(), MessageID: id, Message: "known unsent"}); err != nil {
				t.Fatal(err)
			}
			before, err := s.Store.read()
			if err != nil {
				t.Fatal(err)
			}
			frozen, _ := json.Marshal(before)
			pi.held = true
			authorize := s.AuthorizeEndpoint
			calls := 0
			deny := false
			s.AuthorizeEndpoint = func(ctx context.Context, e EndpointRecord) error {
				calls++
				if deny && e.Pi != nil {
					return piruntime.ErrScope
				}
				return authorize(ctx, e)
			}
			for _, action := range []string{"allow", "send", "reply"} {
				request := Request{Action: action, Peer: to.Slug(), Approval: "user reference", MessageID: strings.Repeat("f", 32), Message: "must remain blocked", ReplyTo: id}
				if _, err := s.RunEndpoint(ctx, from, request); err == nil {
					t.Fatal("delivery operation bypassed readiness", action)
				}
			}
			revoke := Request{Action: "revoke", Peer: to.Slug()}
			prepares := pi.prepares
			deny = true
			if _, err := s.RunEndpoint(ctx, from, revoke); err == nil {
				t.Fatal("revoke bypassed current endpoint admission")
			}
			deny = false
			writer := s.Store.Schema2Write
			s.Store.Schema2Write = nil
			if _, err := s.RunEndpoint(ctx, from, revoke); err == nil {
				t.Fatal("revoke bypassed current package writer")
			}
			s.Store.Schema2Write = writer
			unchanged, err := s.Store.read()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(unchanged)
			if string(data) != string(frozen) {
				t.Fatal("refused operation mutated authorization")
			}
			calls = 0
			view, err := s.RunEndpoint(ctx, from, revoke)
			if err != nil || len(view.Grants) != 1 || view.Grants[0].RevokedAt == nil || view.Messages[0].Status != "canceled" {
				t.Fatal("held pair could not be revoked", err)
			}
			if calls != 2 || pi.prepares != prepares || pi.sends != 0 {
				t.Fatal("revocation skipped admission or invoked delivery readiness", calls, pi.prepares, prepares, pi.sends)
			}
		})
	}
}

type endpointOriginalTransport struct {
	*testTransport
	interrupted bool
	proof       bool
	proofErr    error
	proofCalls  int
	original    Message
}

func (f *endpointOriginalTransport) Send(ctx context.Context, m Message) (codex.SendReceipt, error) {
	if !f.interrupted {
		f.interrupted = true
		f.original = m
		return codex.SendReceipt{}, context.DeadlineExceeded
	}
	if m.ID != f.original.ID || m.Envelope != f.original.Envelope || !reflect.DeepEqual(m.Options, f.original.Options) {
		return codex.SendReceipt{}, errors.New("original changed")
	}
	return f.testTransport.Send(ctx, m)
}
func (f *endpointOriginalTransport) CanSubmitOriginal(_ context.Context, m Message) (bool, error) {
	f.proofCalls++
	return f.proof, f.proofErr
}
func TestTypedMixedCodexOriginalPositiveProofAndCurrentAdmission(t *testing.T) {
	for _, scenario := range []string{"negative", "prepared", "unsupported", "incomplete", "ambiguous", "held", "revoked", "identity", "admission"} {
		t.Run(scenario, func(t *testing.T) {
			s, ids, native, _ := endpointServiceFixture(t)
			ctx := context.Background()
			f := &endpointOriginalTransport{testTransport: native, proof: true}
			s.Transport = f
			if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "allow", Peer: "a", Approval: "user direction"}); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("d", 32)
			if _, err := s.RunEndpoint(ctx, ids["p"], Request{Action: "send", Peer: "a", MessageID: id, Message: "original"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			if native.sends != 0 {
				t.Fatal("canceled before native")
			}
			switch scenario {
			case "unsupported":
				s.Transport = native
			case "incomplete":
				f.proofErr = context.DeadlineExceeded
			case "ambiguous":
				f.proof = false
			case "held":
				native.held = "Recipient execution is held"
			case "revoked":
				if _, err := s.RunEndpoint(ctx, ids["a"], Request{Action: "revoke", Peer: "p"}); err != nil {
					t.Fatal(err)
				}
			case "identity":
				value := *ids["a"].Codex
				value.Incarnation = "replacement"
				ids["a"] = EndpointRecord{Codex: &value}
			case "admission":
				s.AuthorizeEndpoint = func(context.Context, EndpointRecord) error { return errors.New("generation changed") }
			}
			if err := s.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			want := 0
			if scenario == "negative" || scenario == "prepared" {
				want = 1
			}
			if native.sends != want {
				t.Fatal(scenario, native.sends)
			}
			if want == 1 {
				if err := s.Dispatch(ctx, id); err != nil || native.sends != 1 {
					t.Fatal("delivered original repeated", err)
				}
			}
		})
	}
}
