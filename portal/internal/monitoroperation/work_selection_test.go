package monitoroperation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkSelectionRefreshAndAuthority(t *testing.T) {
	ctx := context.Background()
	s, _, req, _ := operationFixture(t)
	op, err := s.Start(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := s.Store.path(op.ID)
	check := func(decoded, observations int) {
		t.Helper()
		view, err := s.Store.Work(ctx)
		if err != nil || view.DecodedRecords != decoded || len(view.Operations) != observations {
			t.Fatalf("selection %+v: %v", view, err)
		}
	}
	check(1, 1)
	check(0, 1)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	// Same-size in-place edits with restored mtime still invalidate through ctime.
	op.Launch.Detail = "edit"
	changed, _ := json.Marshal(op)
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	info, _ = os.Stat(path)
	op.Launch.Detail = "next"
	sameSize, _ := json.Marshal(op)
	if len(sameSize) != len(changed) {
		t.Fatal("not a same-size fixture")
	}
	if err = os.WriteFile(path, sameSize, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	tmp := filepath.Join(s.Store.directory, ".replacement")
	if err = os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	restarted := &Store{directory: s.Store.directory, workspace: s.Store.workspace}
	view, err := restarted.Work(ctx)
	if err != nil || view.DecodedRecords != 1 {
		t.Fatal(view, err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	if err = os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Work(ctx); err == nil {
		t.Fatal("malformed changed record used cached eligibility")
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	// A newly appeared buddy invalidates even an otherwise unchanged native file.
	ci := ciRequest(req)
	ci.ID = strings.Repeat("2", 32)
	record, err := s.StartCIObservation(ctx, "lead", ci)
	if err != nil {
		t.Fatal(err)
	}
	record.ID = op.ID
	recordPath, _ := s.Store.observerPath(op.ID)
	b, _ := json.Marshal(record)
	if err = os.WriteFile(recordPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Work(ctx); err == nil {
		t.Fatal("paired duplicate identity escaped refresh")
	}
}

func TestWorkSelectionTerminalDeliveryAndPollGuard(t *testing.T) {
	ctx := context.Background()
	s, _, req, _ := operationFixture(t)
	req = ciRequest(req)
	record, err := s.StartCIObservation(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.lockObserver(ctx, record.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	op := observerEventOperation(*tx.record)
	exit := 0
	if err = AddEvent(&op, ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tx.record.Events = op.Events
	bumpObserver(tx.record, time.Now())
	if err = tx.save(); err != nil {
		t.Fatal(err)
	}
	tx.close()
	for _, status := range []string{"pending", "held", "submitting"} {
		tx, err = s.Store.lockObserver(ctx, record.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		tx.record.Events[0].Delivery.Status = status
		bumpObserver(tx.record, time.Now())
		if err = tx.save(); err != nil {
			t.Fatal(err)
		}
		tx.close()
		view, err := s.Store.Work(ctx)
		if err != nil || len(view.Observers) != 0 || len(view.Events) != 1 {
			t.Fatal(view, err)
		}
	}
	s.Resolve = func(string) (Owner, error) {
		t.Fatal("terminal CI event reached owner resolution")
		return Owner{}, nil
	}
	s.ObserverEpoch = strings.Repeat("e", 32)
	if _, err = s.ClaimCIPoll(ctx, record.ID); err != ErrPollNotDue {
		t.Fatal(err)
	}
}

func TestWorkSelectionDoesNotAuthorizeStaleDelivery(t *testing.T) {
	ctx := context.Background()
	s, _, req, owner := operationFixture(t)
	op, err := s.Start(ctx, "lead", req)
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	op, err = s.Report(ctx, op.Launch.ThreadID, ReportRequest{Slot: "terminal", Outcome: "passed", ExitStatus: &exit})
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.Store.Work(ctx)
	if err != nil || len(view.Events) != 1 {
		t.Fatal(view, err)
	}
	transport := &deliveryFixtureTransport{store: s.Store, t: t}
	delivery := &DeliveryService{Store: s.Store, Resolve: s.Resolve, LockSession: s.LockSession, Transport: transport}
	owner.RootThreadID = "replacement-lead"
	ref := view.Events[0]
	_ = delivery.Dispatch(ctx, ref.OperationID, ref.EventID)
	if transport.sends != 0 || transport.prepares != 0 {
		t.Fatal("stale scheduling advice authorized replacement owner")
	}
}
