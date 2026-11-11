package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

func TestOperationNewTerminalReportsRequireEmptyStillRunning(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		rejected      bool
		unresolved    bool
	}{
		{"passed omitted", `{"slot":"terminal","outcome":"passed","exitStatus":0}`, false, false},
		{"passed empty", `{"slot":"terminal","outcome":"passed","exitStatus":0,"stillRunning":""}`, false, false},
		{"canceled omitted", `{"slot":"terminal","outcome":"failed","exitStatus":1,"conclusion":"cancelled","detail":"Observation session exited; CI run completed/cancelled","evidence":["run/attempt/1"]}`, false, false},
		{"canceled empty", `{"slot":"terminal","outcome":"failed","exitStatus":1,"conclusion":"cancelled","stillRunning":""}`, false, false},
		{"passed none", `{"slot":"terminal","outcome":"passed","exitStatus":0,"stillRunning":"none"}`, true, false},
		{"failed none prose", `{"slot":"terminal","outcome":"failed","exitStatus":1,"conclusion":"cancelled","stillRunning":"none; observation session 68962 exited 1; GitHub run 42816255766 attempt 1 is terminal completed/cancelled"}`, true, false},
		{"failed running", `{"slot":"terminal","outcome":"failed","exitStatus":1,"stillRunning":"tool session 68962"}`, true, false},
		{"passed whitespace", `{"slot":"terminal","outcome":"passed","exitStatus":0,"stillRunning":" "}`, true, false},
		{"incomplete unknown", `{"slot":"terminal","outcome":"incomplete","detail":"Lost tool session status","stillRunning":"tool session 68962"}`, false, true},
		{"incomplete without identity", `{"slot":"terminal","outcome":"incomplete","detail":"Operation-owned effects remain unknown"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, _, req, _ := operationFixture(t)
			op, err := s.Start(ctx, "lead", req)
			if err != nil {
				t.Fatal(err)
			}
			var report ReportRequest
			if err := json.Unmarshal([]byte(tc.payload), &report); err != nil {
				t.Fatal(err)
			}
			// The cold observer and utility API share new-event admission.
			candidate := cloneOperation(op)
			directErr := AddEvent(&candidate, report, time.Now())
			_, reportErr := s.Report(ctx, op.Launch.ThreadID, report)
			for _, err := range []error{directErr, reportErr} {
				if tc.rejected {
					if err == nil || !strings.Contains(err.Error(), "empty or omitted stillRunning") || !strings.Contains(err.Error(), "incomplete") {
						t.Fatalf("contradictory report lacks actionable rejection: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			stored, err := s.Store.Get(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.rejected {
				if len(stored.Events) != 0 || !reflect.DeepEqual(candidate, op) {
					t.Fatal("rejected report changed the operation")
				}
			} else if len(stored.Events) != 1 || HasUnresolvedEffects(stored) != tc.unresolved {
				t.Fatalf("accepted report has incorrect event or effects fence: %#v", stored.Events)
			}
		})
	}
}

func TestOperationHistoricalContradictoryReportRemainsImmutableAndFenced(t *testing.T) {
	ctx := context.Background()
	s, client, lifecycle, op := lifecycleFixture(t)
	exit := 1
	report := ReportRequest{Slot: "terminal", Outcome: "failed", ExitStatus: &exit, Conclusion: "cancelled",
		StillRunning: "none; observation session 68962 exited 1; GitHub run 42816255766 attempt 1 is terminal completed/cancelled"}
	id := derivedID(op.ID, report.Slot)
	op.Events = []Event{{ID: id, CreatedAt: time.Now().UTC(), Report: report, Delivery: Delivery{
		Status: "accepted", MessageID: id, Context: "monitor:" + op.ID + ":" + id, Envelope: "Retained historical report",
		Receipt: &codex.SendReceipt{ClientUserMessageID: id, TurnID: "historical-delivery"},
	}}}
	// Write a retained record directly: new-event admission must reject this
	// exact historical shape without invalidating its persisted contract.
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Store.path(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	retained, err := s.Store.Get(ctx, op.ID)
	if err != nil || !reflect.DeepEqual(retained.Events, op.Events) || !HasUnresolvedEffects(retained) {
		t.Fatalf("historical report became unreadable, changed or lost its fence: %v", err)
	}
	if err := AddEvent(&retained, report, time.Now()); err != nil || !reflect.DeepEqual(retained.Events, op.Events) {
		t.Fatalf("cold observer exact replay changed historical event: %v", err)
	}
	again, err := s.Report(ctx, op.Launch.ThreadID, report)
	if err != nil || !reflect.DeepEqual(again.Events, op.Events) || !HasUnresolvedEffects(again) {
		t.Fatalf("utility exact replay changed historical event or fence: %v", err)
	}
	changed := report
	changed.StillRunning = ""
	if _, err := s.Report(ctx, op.Launch.ThreadID, changed); err == nil {
		t.Fatal("retry rewrote the immutable historical report")
	}
	if _, err := s.Report(ctx, "foreign-utility", report); err == nil {
		t.Fatal("historical replay bypassed caller binding")
	}
	owner, err := s.Resolve(op.Owner.Slug)
	if err != nil {
		t.Fatal(err)
	}
	s.Resolve = func(string) (Owner, error) {
		changed := owner
		changed.SocketEpoch = "new-epoch"
		return changed, nil
	}
	if _, err := s.Report(ctx, op.Launch.ThreadID, report); err == nil {
		t.Fatal("historical replay bypassed epoch binding")
	}
	s.Resolve = func(string) (Owner, error) { return owner, nil }
	if err := lifecycle.RequireIdle(ctx, op.Owner.Slug, op.Owner.RootThreadID, false); err == nil {
		t.Fatal("historical none prose bypassed the external-effects fence")
	}
	resolution := ResolutionRequest{OperationID: op.ID, ResolutionID: strings.Repeat("b", 32),
		Evidence: []string{"exact source, session 68962 and operation-owned cells reconciled"}, Detail: "All known effects accounted for"}
	client.idleError = errors.New("native utility remains active")
	if _, err := s.ResolveEffects(ctx, "lead", resolution); err == nil {
		t.Fatal("historical report resolution bypassed native proof")
	}
	client.idleError = nil
	resolved, err := s.ResolveEffects(ctx, "lead", resolution)
	if err != nil || HasUnresolvedEffects(resolved) || !reflect.DeepEqual(resolved.Events, op.Events) {
		t.Fatalf("evidence resolution failed or changed historical outcome/receipt: %v", err)
	}
	if err := lifecycle.RequireIdle(ctx, op.Owner.Slug, op.Owner.RootThreadID, false); err != nil {
		t.Fatal(err)
	}
	if client.starts != 1 || client.sends != 1 || client.archives != 0 {
		t.Fatal("report retry or resolution caused native execution or archival")
	}
}
