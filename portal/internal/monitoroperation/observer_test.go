package monitoroperation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ciRequest(req StartRequest) StartRequest {
	req.Source = Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}
	return req
}

func TestDirectCIRegistrationDoesNotLaunchNativeAndReplaysOriginalDriver(t *testing.T) {
	ctx := context.Background()
	s, native, req, _ := operationFixture(t)
	req = ciRequest(req)
	result, err := s.StartManaged(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	view, ok := result.(ObserverStatus)
	if !ok || view.ExecutionOwner != "runtime-ci" {
		t.Fatalf("wrong execution owner: %#v", result)
	}
	data, _ := json.Marshal(view)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	if fields["launch"] != nil || fields["utilityPolicy"] != nil || native.creates != 0 || native.starts != 0 || native.sends != 0 {
		t.Fatal("direct observation fabricated or launched native execution")
	}
	path, _ := s.Store.path(req.ID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("direct registration created native state", err)
	}
	again, err := s.StartManaged(ctx, "lead", req)
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatal("direct replay changed registration", err)
	}
	changed := req
	changed.Source.RunAttempt++
	if _, err := s.StartManaged(ctx, "lead", changed); err == nil {
		t.Fatal("changed source reused direct identity")
	}
	if _, err := s.Report(ctx, "lead", ReportRequest{Slot: "terminal", Outcome: "incomplete", Detail: "invalid reporter"}); err == nil {
		t.Fatal("direct observer accepted utility report")
	}
	other, f, legacyReq, _ := operationFixture(t)
	legacyReq = ciRequest(legacyReq)
	op, err := other.Start(ctx, "lead", legacyReq)
	if err != nil {
		t.Fatal(err)
	}
	old, err := other.StartManaged(ctx, "lead", legacyReq)
	if err != nil {
		t.Fatal(err)
	}
	oldView, ok := old.(NativeStatus)
	if !ok || oldView.ExecutionOwner != "native" || oldView.Launch.ThreadID != op.Launch.ThreadID || f.starts != 1 {
		t.Fatal("legacy CI replay changed its execution driver")
	}
	unsupported := req
	unsupported.ID = strings.Repeat("2", 32)
	unsupported.Source.Provider = "unsupported"
	if _, err := s.StartManaged(ctx, "lead", unsupported); err == nil || native.starts != 0 {
		t.Fatal("unsupported CI silently launched native fallback")
	}
}

func TestObserverStoreReadIsStrictAndAbsentStateCreatesNothing(t *testing.T) {
	ctx := context.Background()
	s, _, req, _ := operationFixture(t)
	req = ciRequest(req)
	if records, err := s.Store.ListObservers(ctx, ""); err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
	if _, err := os.Stat(s.Store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent observer read created state")
	}
	record, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := s.Store.observerPath(record.ID)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(valid), `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(string(valid), `"schema":1`, `"schema":2`, 1), strings.Replace(string(valid), `"schema":1`, `"extra":true,"schema":1`, 1)} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.GetObserver(ctx, record.ID); err == nil {
			t.Fatal("invalid observer accepted")
		}
		if _, err := s.Store.ListObservers(ctx, "unrelated-owner"); err == nil {
			t.Fatal("invalid foreign observer accepted by listing")
		}
	}
	if err := os.WriteFile(path, valid, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.GetObserver(ctx, record.ID); err == nil {
		t.Fatal("nonprivate observer accepted")
	}
	if _, err := s.Store.ListObservers(ctx, "unrelated-owner"); err == nil {
		t.Fatal("nonprivate foreign observer accepted by listing")
	}
}

func TestObserverRecordDecoderPreservesStrictValidation(t *testing.T) {
	s, _, req, _ := operationFixture(t)
	record, err := s.StartCIObservation(context.Background(), "lead", ciRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeObserverRecord(valid)
	if err != nil || !reflect.DeepEqual(decoded, record) {
		t.Fatal("pure decoder changed the owning record", err)
	}
	for name, data := range map[string][]byte{
		"malformed": []byte("{"),
		"schema":    []byte(strings.Replace(string(valid), `"schema":1`, `"schema":2`, 1)),
		"duplicate": []byte(strings.Replace(string(valid), `"schema":1`, `"schema":1,"schema":1`, 1)),
		"unknown":   []byte(strings.Replace(string(valid), `"schema":1`, `"extra":true,"schema":1`, 1)),
		"trailing":  append(append([]byte{}, valid...), []byte(" {}")...),
		"oversize":  []byte(strings.Repeat(" ", maxOperationBytes) + string(valid)),
		"owner":     []byte(strings.Replace(string(valid), `"rootThreadId":"lead"`, `"rootThreadId":""`, 1)),
		"source":    []byte(strings.Replace(string(valid), `"kind":"ci"`, `"kind":"local"`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeObserverRecord(data); err == nil {
				t.Fatal("invalid observer accepted")
			}
		})
	}
}

type joinedCIObserver struct {
	service *Service
	claim   ObserverRecord
	receipt PollReceipt
	calls   int
}

func (o *joinedCIObserver) Poll(context.Context, Source) (CISnapshot, PollReceipt, error) {
	return CISnapshot{}, PollReceipt{}, errors.New("unexpected poll")
}
func (o *joinedCIObserver) OwnsPoll(id string, p PollReceipt) bool {
	return id == o.claim.ID && p.ID == o.claim.Poll.ID && p.OwnerEpoch == o.claim.Poll.OwnerEpoch
}
func (o *joinedCIObserver) Quiesce(ctx context.Context, id string) (PollReceipt, error) {
	o.calls++
	if id != o.claim.ID {
		return PollReceipt{}, errors.New("wrong operation")
	}
	_, err := o.service.CommitCIPoll(ctx, o.claim, &CISnapshot{Status: "queued", ObservedAt: o.receipt.StartedAt, SourceDigest: o.claim.SourceDigest}, o.receipt, "")
	return o.receipt, err
}

func TestDirectPollQuiescenceJoinsOutsideLockAndNeverCancelsRemoteOrWakesLate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, native, req, owner := operationFixture(t)
	req = ciRequest(req)
	s.ObserverEpoch = "process-epoch"
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	record, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCIPoll(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCIPoll(ctx, record.ID); !errors.Is(err, ErrPollNotDue) {
		t.Fatal("duplicate poll admitted", err)
	}
	receipt := *claim.Poll
	receipt.State = "running"
	receipt.Handle = "exact-owned-receipt"
	if _, err := s.CommitCIPoll(ctx, claim, nil, receipt, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	receipt.State = "joined"
	finished := now
	receipt.FinishedAt = &finished
	exit := 1
	receipt.ExitStatus = &exit
	adapter := &joinedCIObserver{service: s, claim: claim, receipt: receipt}
	a := ArchiveAuthority{Slug: owner.Slug, RootThreadID: owner.RootThreadID, OperationID: strings.Repeat("8", 64), TargetID: "exact-v2-target", Mode: "explicit"}
	lifecycle := Lifecycle{Store: s.Store, Observer: adapter, Now: s.Now, AuthorizeArchive: func(_ context.Context, got ArchiveAuthority) error {
		if got != a {
			return errors.New("wrong accepted archive")
		}
		return nil
	}}
	view, err := lifecycle.AssessLocalSafety(ctx, owner.Slug, owner.RootThreadID)
	if err != nil || view.State != "quiesceable" {
		t.Fatal(view, err)
	}
	if err := lifecycle.BeginArchiveQuiescence(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCIPoll(ctx, record.ID); !errors.Is(err, ErrPollNotDue) {
		t.Fatal("archive intent allowed another read", err)
	}
	view, err = lifecycle.CompleteArchiveQuiescence(ctx, a)
	if err != nil || view.State != "quiescent" {
		t.Fatal(view, err)
	}
	detached, err := s.Store.GetObserver(ctx, record.ID)
	if err != nil || detached.Phase != "detached" || detached.Remote == nil || detached.Remote.Status != "queued" || len(detached.Events) != 0 || adapter.calls != 1 {
		t.Fatal(detached, err)
	}
	late := CISnapshot{Status: "completed", Conclusion: "success", ObservedAt: now, SourceDigest: claim.SourceDigest}
	if _, err := s.CommitCIPoll(ctx, claim, &late, receipt, ""); err != nil {
		t.Fatal(err)
	}
	detached, _ = s.Store.GetObserver(ctx, record.ID)
	if len(detached.Events) != 0 || detached.Remote.Status != "queued" || native.starts != 0 || native.sends != 0 {
		t.Fatal("late result reopened/woke detached observer")
	}
}

func joinedPoll(claim ObserverRecord, at time.Time) PollReceipt {
	r := *claim.Poll
	r.State, r.Handle, r.FinishedAt = "joined", "owned:"+r.ID, &at
	exit := 0
	r.ExitStatus = &exit
	return r
}

func TestDirectCancelKeepsObservationAndSeparatesSameSourceCalls(t *testing.T) {
	ctx := context.Background()
	s, native, req, _ := operationFixture(t)
	s.ObserverEpoch = "worker"
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	req = ciRequest(req)
	a, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	other := req
	other.ID = strings.Repeat("9", 32)
	b, err := s.StartCIObservation(ctx, "lead", other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelCIObservation(ctx, "lead", a.ID); err != nil {
		t.Fatal(err)
	}
	ca, err := s.ClaimCIPoll(ctx, a.ID)
	if err != nil {
		t.Fatal("continuation cancellation stopped observation", err)
	}
	cb, err := s.ClaimCIPoll(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.CommitCIPoll(ctx, ca, nil, joinedPoll(cb, now), ""); err == nil {
		t.Fatal("same-source operation adopted another call")
	}
	wrong := CISnapshot{Status: "completed", Conclusion: "success", ObservedAt: now, SourceDigest: strings.Repeat("0", 64)}
	if _, err := s.CommitCIPoll(ctx, ca, &wrong, joinedPoll(ca, now), ""); err == nil {
		t.Fatal("changed source accepted")
	}
	queued := CISnapshot{Status: "queued", ObservedAt: now, SourceDigest: ca.SourceDigest}
	if _, err := s.CommitCIPoll(ctx, ca, &queued, joinedPoll(ca, now), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCIPoll(ctx, a.ID); !errors.Is(err, ErrPollNotDue) {
		t.Fatal("minimum polling interval bypassed", err)
	}
	now = now.Add(15 * time.Second)
	ca, err = s.ClaimCIPoll(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	terminal := CISnapshot{Status: "completed", Conclusion: "success", ObservedAt: now, SourceDigest: ca.SourceDigest}
	finished, err := s.CommitCIPoll(ctx, ca, &terminal, joinedPoll(ca, now), "")
	if err != nil || len(finished.Events) != 1 || finished.Events[0].Delivery.Status != "canceled" || finished.Events[0].Report.Outcome != "passed" {
		t.Fatal(finished, err)
	}
	retained, err := s.Store.GetObserver(ctx, b.ID)
	if err != nil || retained.Poll.State != "intent" || retained.Poll.ID != cb.Poll.ID || retained.Remote != nil || native.sends != 0 || native.starts != 0 {
		t.Fatal("same-source record leaked proof", retained, err)
	}
}

func TestObserverCrashIntentAndAcceptedBindingStayFenced(t *testing.T) {
	ctx := context.Background()
	s, _, req, owner := operationFixture(t)
	s.ObserverEpoch = "lost-worker"
	req = ciRequest(req)
	record, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCIPoll(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	l := Lifecycle{Store: s.Store, AuthorizeArchive: func(context.Context, ArchiveAuthority) error { return nil }}
	a := ArchiveAuthority{Slug: owner.Slug, RootThreadID: owner.RootThreadID, OperationID: strings.Repeat("8", 64), TargetID: "v2-target", Mode: "explicit"}
	unknown, err := l.AssessLocalSafety(ctx, owner.Slug, owner.RootThreadID)
	if err != nil || unknown.State != "blocked" {
		t.Fatal(unknown, err)
	}
	if err := (Lifecycle{Store: s.Store}).BeginArchiveQuiescence(ctx, a); err == nil {
		t.Fatal("bare authority accepted")
	}
	if err := l.BeginArchiveQuiescence(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		view, err := l.CompleteArchiveQuiescence(ctx, a)
		if err != nil || view.State != "blocked" {
			t.Fatal("lost handle inferred joined", view, err)
		}
	}
	restarted := *s
	restarted.ObserverEpoch = "new-worker"
	if _, err := restarted.ClaimCIPoll(ctx, record.ID); !errors.Is(err, ErrPollNotDue) {
		t.Fatal("crash intent was restarted", err)
	}
	inv, err := s.Store.Inventory(ctx, owner.Slug, owner.RootThreadID)
	if err != nil || len(inv.Unresolved) == 0 {
		t.Fatal("transition inventory lost uncertain poll", inv, err)
	}
	joined := joinedPoll(claim, time.Now().UTC().Add(time.Second))
	if _, err := s.CommitCIPoll(ctx, claim, nil, joined, "joined original execution"); err != nil {
		t.Fatal(err)
	}
	view, err := l.CompleteArchiveQuiescence(ctx, a)
	if err != nil || view.State != "quiescent" {
		t.Fatal(view, err)
	}
	if err := l.BeginArchiveQuiescence(ctx, a); err != nil {
		t.Fatal("exact detached retry refused", err)
	}
	other := a
	other.OperationID = strings.Repeat("7", 64)
	if err := l.BeginArchiveQuiescence(ctx, other); err == nil {
		t.Fatal("detached observer rebound to another archive")
	}
	if _, err := l.CompleteArchiveQuiescence(ctx, other); err == nil {
		t.Fatal("different archive reused detached proof")
	}
	inv, err = s.Store.Inventory(ctx, owner.Slug, owner.RootThreadID)
	if err != nil || len(inv.Unresolved) != 0 {
		t.Fatal(inv, err)
	}
}

func TestCrossFormatCollisionIsCorruptionForBothReaders(t *testing.T) {
	ctx := context.Background()
	s, _, req, _ := operationFixture(t)
	req = ciRequest(req)
	if _, err := s.StartCIObservation(ctx, "lead", req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, "lead", req); err == nil {
		t.Fatal("native creation overwrote direct identity")
	}
	legacy, _, oldReq, _ := operationFixture(t)
	op, err := legacy.Start(ctx, "lead", ciRequest(oldReq))
	if err != nil {
		t.Fatal(err)
	}
	op.Workspace = s.Store.workspace
	if err := s.Store.save(op); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.GetObserver(ctx, req.ID); err == nil {
		t.Fatal("direct reader ignored native collision")
	}
	if _, err := s.Store.Get(ctx, req.ID); err == nil {
		t.Fatal("native reader ignored direct collision")
	}
	for _, slug := range []string{"monitor-test", "unrelated-owner"} {
		if _, err := s.Store.ListObservers(ctx, slug); err == nil {
			t.Fatal("listing ignored native/observer collision before filtering", slug)
		}
	}
}

// Archive IDs belong to the ordinary lifecycle journal contract, independently
// of monitor operation IDs. Real archive callers use 32 random bytes.
func TestObserverArchiveAuthorityUsesLifecycleJournalID(t *testing.T) {
	a := ArchiveAuthority{Slug: "monitor-test", RootThreadID: "lead", OperationID: strings.Repeat("a", 64), TargetID: "v2-target", Mode: "archived"}
	if err := validateArchive(a); err != nil {
		t.Fatal("ordinary lifecycle journal ID refused", err)
	}
	if validID(a.OperationID) {
		t.Fatal("monitor operation ID contract widened to lifecycle IDs")
	}
	for _, id := range []string{"", strings.Repeat("a", 32), "11111111-1111-1111-1111-111111111111", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("a", 63) + "g"} {
		bad := a
		bad.OperationID = id
		if err := validateArchive(bad); err == nil {
			t.Fatalf("noncanonical lifecycle ID accepted: %q", id)
		}
	}
}

func TestDirectCIAdmissionRejectsAdapterInvalidSourcesBeforeRegistration(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*Source)
	}{
		{"workflow name", func(s *Source) { s.WorkflowID = "Integration Tests" }},
		{"run text", func(s *Source) { s.RunID = "run-34" }},
		{"zero run", func(s *Source) { s.RunID = "0" }},
		{"noncanonical workflow", func(s *Source) { s.WorkflowID = "012" }},
		{"short commit", func(s *Source) { s.Commit = "abc" }},
		{"uppercase commit", func(s *Source) { s.Commit = strings.Repeat("A", 40) }},
		{"repository path", func(s *Source) { s.Repository = "owner/project/extra" }},
		{"dot owner", func(s *Source) { s.Repository = "../project" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			ctx := context.Background()
			s, native, req, _ := operationFixture(t)
			req = ciRequest(req)
			change.apply(&req.Source)
			if err := validateStart(req); err != nil {
				t.Fatal("fixture must retain generic source compatibility", err)
			}
			if _, err := s.StartManaged(ctx, "lead", req); err == nil {
				t.Fatal("adapter-invalid new source registered")
			}
			if records, err := s.Store.ListObservers(ctx, ""); err != nil || len(records) != 0 {
				t.Fatal("invalid registration left observer/poll state", records, err)
			}
			path, _ := s.Store.observerPath(req.ID)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid source persisted", err)
			}
			if native.creates != 0 || native.starts != 0 || native.sends != 0 {
				t.Fatal("invalid source produced native effects")
			}
		})
	}
}

func TestDirectAdmissionPreservesExactHistoricalNativeAndStoredReplay(t *testing.T) {
	ctx := context.Background()
	s, native, req, _ := operationFixture(t)
	req = ciRequest(req)
	req.Source.WorkflowID, req.Source.RunID, req.Source.Commit = "Integration Tests", "historical-run", "retained-commit"
	op, err := s.Start(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.Report(ctx, op.Launch.ThreadID, ReportRequest{Slot: "terminal", Outcome: "incomplete", Detail: "Historical CI observer; local proof remains separate", StillRunning: "exact old remote source"})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := s.Store.path(op.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.Store.Get(ctx, op.ID)
	if err != nil || !reflect.DeepEqual(read.Events, op.Events) {
		t.Fatal("historical source became unreadable", read, err)
	}
	result, err := s.StartManaged(ctx, "lead", req)
	if err != nil {
		t.Fatal("exact historical native replay tightened", err)
	}
	if got, ok := result.(NativeStatus); !ok || got.ExecutionOwner != "native" || !reflect.DeepEqual(got.Source, req.Source) {
		t.Fatal(result)
	}
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(before, after) || native.creates != 1 || native.starts != 1 || native.sends != 1 {
		t.Fatal("native replay changed bytes or repeated effects")
	}
	// A retained direct record also replays its exact source before new-only
	// admission, without making its historical generic reader stricter.
	direct, _, directReq, _ := operationFixture(t)
	directReq = ciRequest(directReq)
	r, err := direct.StartCIObservation(ctx, "lead", directReq)
	if err != nil {
		t.Fatal(err)
	}
	directReq.Source.WorkflowID = "Retained workflow name"
	r.Source = directReq.Source
	r.SourceDigest = SourceDigest(r.Source)
	r.RequestDigest = digest(directReq)
	storedPath, _ := direct.Store.observerPath(r.ID)
	data, _ := json.Marshal(r)
	if err := os.WriteFile(storedPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Store.GetObserver(ctx, r.ID); err != nil {
		t.Fatal("stored generic source no longer readable", err)
	}
	replayed, err := direct.StartCIObservation(ctx, "lead", directReq)
	if err != nil || !reflect.DeepEqual(replayed, r) {
		t.Fatal("stored exact direct replay tightened", replayed, err)
	}
}

func TestNativeStatusWireContract(t *testing.T) {
	ctx := context.Background()
	service, _, request, _ := operationFixture(t)
	op, err := service.Start(ctx, "lead", request)
	if err != nil || op.Launch.Receipt == nil {
		t.Fatal("native fixture did not produce its actual receipt", err)
	}
	// Exercise optional nested fields too; envelope handling must not project them.
	at, options, exit := op.CreatedAt, op.Launch.Options, 1
	op.CancellationScope = "only the original invocation"
	op.Source.EnvironmentReferences = []string{"DECLARED_INPUT"}
	op.CanceledAt, op.Disposition = &at, "resolved"
	op.Launch.TerminatedAt, op.Launch.Termination, op.Launch.Detail = &at, "exited", "exact native receipt"
	op.Events = []Event{{ID: "event", CreatedAt: at,
		Report: ReportRequest{Slot: "terminal", Outcome: "failed", ExitStatus: &exit, ElapsedMillis: 23,
			Evidence: []string{"exact source exit"}, Detail: "bounded failure"},
		Delivery: Delivery{Status: "accepted", MessageID: "delivery", Envelope: "saved message", Context: "original context",
			Options: &options, Receipt: op.Launch.Receipt, AdmittedEpoch: "original-epoch", Detail: "accepted"}}}
	op.Resolution = &Resolution{ID: "resolution", At: at, LeadThreadID: op.Owner.RootThreadID,
		Evidence: []string{"original joined tool"}, Detail: "local effects proved"}
	status, err := service.AcknowledgeNative(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(status); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || string(fields["executionOwner"]) != `"native"` || fields["operation"] != nil {
		t.Fatal("native envelope lost its flat discriminator", string(data), err)
	}
	var decoded NativeStatus
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded, status) {
		t.Fatal("native envelope changed fields or receipts", decoded, status, err)
	}
	persisted, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var round Operation
	if err := json.Unmarshal(persisted, &round); err != nil || !reflect.DeepEqual(round, op) {
		t.Fatal("standalone native codec changed", err)
	}
	_, pi, _ := piOperationFixture(t)
	piData, err := json.Marshal(Operation{Schema: 2, ID: pi.ID, Workspace: pi.Workspace, Pi: &pi})
	if err != nil {
		t.Fatal(err)
	}
	bad := []struct{ name, data string }{
		{"malformed", `{`}, {"unknown", strings.Replace(string(data), `{`, `{"unsupported":true,`, 1)},
		{"trailing", string(data) + `{}`}, {"missing discriminator", string(persisted)},
		{"wrong discriminator", strings.Replace(string(data), `"executionOwner":"native"`, `"executionOwner":"runtime-ci"`, 1)},
		{"missing schema", strings.Replace(string(data), `"schema":1,`, ``, 1)},
		{"wrong schema", strings.Replace(string(data), `"schema":1`, `"schema":2`, 1)},
		{"Pi envelope", strings.Replace(string(piData), `{`, `{"executionOwner":"native",`, 1)},
	}
	for _, item := range bad {
		t.Run(item.name, func(t *testing.T) {
			retained := status
			if err := json.Unmarshal([]byte(item.data), &retained); err == nil || !reflect.DeepEqual(retained, status) {
				t.Fatal("invalid native envelope accepted or assigned before validation", item.data, err)
			}
		})
	}
	for _, invalid := range []NativeStatus{{Operation: op, ExecutionOwner: "runtime-ci"},
		{Operation: Operation{Schema: 2, Pi: &pi}, ExecutionOwner: "native"}} {
		if _, err := json.Marshal(invalid); err == nil {
			t.Fatal("marshaled a non-native envelope")
		}
	}
}

func TestObserverListReadsAtomicSnapshotUnderUnrelatedOwnerLock(t *testing.T) {
	service, native, req, _ := operationFixture(t)
	record, err := service.StartCIObservation(context.Background(), "lead", ciRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	const unrelated = "unrelated-owner"
	if record.Owner.Slug == unrelated {
		t.Fatal("control owners overlap")
	}
	path, _ := service.Store.observerPath(record.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := service.Store.lockObserver(context.Background(), record.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if records, err := service.Store.ListObservers(ctx, unrelated); err != nil || len(records) != 0 {
		t.Fatal("unrelated observer lock delayed validated target-empty listing", records, err)
	}
	// The snapshot completed while the real writer lock remained held.
	tx.close()
	records, err := service.Store.ListObservers(context.Background(), unrelated)
	if err != nil || len(records) != 0 {
		t.Fatal("released unrelated lock changed slug filtering", records, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || native.creates != 0 || native.starts != 0 || native.sends != 0 {
		t.Fatal("read control changed record or native state", err)
	}
	t.Log("validated listing completes under the real unrelated-owner writer lock without changing bytes or native effects")
}

func TestObserverListAtomicSnapshotRetainsOwnedInflightBlocker(t *testing.T) {
	ctx := context.Background()
	s, native, req, owner := operationFixture(t)
	s.ObserverEpoch = "worker"
	record, err := s.StartCIObservation(ctx, "lead", ciRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCIPoll(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := s.Store.observerPath(record.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.lockObserver(ctx, record.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.close()
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	records, err := s.Store.ListObservers(bounded, owner.Slug)
	if err != nil || len(records) != 1 || records[0].Poll == nil || !reflect.DeepEqual(records[0].Poll, claim.Poll) {
		t.Fatal("inflight snapshot changed persisted receipt", records, err)
	}
	inventory, err := s.Store.Inventory(bounded, owner.Slug, owner.RootThreadID)
	if err != nil || !reflect.DeepEqual(inventory.Unresolved, []string{record.ID}) {
		t.Fatal("atomic discovery waived owned inflight inventory", inventory, err)
	}
	local, err := (Lifecycle{Store: s.Store}).AssessLocalSafety(bounded, owner.Slug, owner.RootThreadID)
	if err != nil || local.State != "blocked" || !reflect.DeepEqual(local.OperationIDs, []string{record.ID}) || local.Diagnostic == nil || local.Diagnostic.Stage != "receipt-proof" {
		t.Fatal("atomic discovery inferred native settlement", local, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || native.creates != 0 || native.starts != 0 || native.sends != 0 {
		t.Fatal("discovery changed receipt or native effects", err)
	}
}

func TestObserverListValidatesForeignSnapshotAndEntries(t *testing.T) {
	for _, kind := range []string{"oversize", "malformed", "workspace", "filename", "invalid-id", "directory", "unexpected-entry", "native-malformed", "root-file"} {
		t.Run(kind, func(t *testing.T) {
			s, _, req, _ := operationFixture(t)
			record, err := s.StartCIObservation(context.Background(), "lead", ciRequest(req))
			if err != nil {
				t.Fatal(err)
			}
			path, _ := s.Store.observerPath(record.ID)
			switch kind {
			case "oversize":
				err = os.WriteFile(path, []byte(strings.Repeat(" ", maxOperationBytes+1)), 0600)
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0600)
			case "workspace":
				record.Workspace = s.Store.workspace + "/foreign"
				data, _ := json.Marshal(record)
				err = os.WriteFile(path, data, 0600)
			case "filename":
				err = os.Rename(path, filepath.Join(filepath.Dir(path), strings.Repeat("2", 32)+".json"))
			case "invalid-id":
				err = os.Rename(path, filepath.Join(filepath.Dir(path), "invalid.json"))
			case "directory":
				err = os.Mkdir(filepath.Join(filepath.Dir(path), "entry.json"), 0700)
			case "unexpected-entry":
				err = os.WriteFile(filepath.Join(filepath.Dir(path), "unexpected"), []byte("ignored?"), 0600)
			case "native-malformed":
				nativePath, _ := s.Store.path(record.ID)
				err = os.WriteFile(nativePath, []byte("{"), 0600)
			case "root-file":
				dir := filepath.Dir(path)
				err = os.Rename(dir, dir+"-saved")
				if err == nil {
					err = os.WriteFile(dir, []byte("invalid directory"), 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Store.ListObservers(context.Background(), "unrelated-owner"); err == nil {
				t.Fatal("invalid foreign snapshot skipped before validation", kind)
			}
		})
	}
}

func TestObserverListSnapshotCancellationOrderAndAbsentStore(t *testing.T) {
	s, native, req, _ := operationFixture(t)
	if list, err := s.Store.ListObservers(context.Background(), ""); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	if _, err := os.Stat(s.Store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent listing created store", err)
	}
	req = ciRequest(req)
	req.ID = strings.Repeat("9", 32)
	if _, err := s.StartCIObservation(context.Background(), "lead", req); err != nil {
		t.Fatal(err)
	}
	req.ID = strings.Repeat("1", 32)
	if _, err := s.StartCIObservation(context.Background(), "lead", req); err != nil {
		t.Fatal(err)
	}
	path, _ := s.Store.observerPath(req.ID)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".observer-pending"), []byte("unpublished"), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := s.Store.ListObservers(context.Background(), "")
	if err != nil || len(list) != 2 || list[0].ID != strings.Repeat("1", 32) || list[1].ID != strings.Repeat("9", 32) {
		t.Fatal("snapshot order changed", list, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Store.ListObservers(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled snapshot proceeded", err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := s.Store.ListObservers(expired, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired snapshot proceeded", err)
	}
	if native.starts != 0 || native.sends != 0 {
		t.Fatal("snapshot launched native effects")
	}
}
