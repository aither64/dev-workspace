package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
)

const controlOperationID = "11223344-1122-4122-8122-112233441122"

func maintenanceWorkerFixture(t *testing.T, samePackage bool) (maintenanceControl, maintenance.Operation) {
	t.Helper()
	root := t.TempDir()
	m := maintenanceControl{store: maintenance.Store{Root: root, Profile: filepath.Join(root, "profile")}, workspace: root}
	target := "/candidate"
	if samePackage {
		target = "/source"
	}
	prep, _ := json.Marshal(map[string]any{"sourcePackage": "/source", "sourceCodex": "/source-native", "sourcePlansDigest": "source-plans", "candidatePackage": target, "candidateCodex": "/target-native", "plansDigest": "target-plans"})
	o := maintenance.Operation{Schema: maintenance.Schema, OperationID: controlOperationID, Revision: 1, Phase: "stopping-native", Profile: m.store.Profile, SourceProfileToken: "source-token", SourcePackage: "/source", CandidatePackage: target, PlansDigest: "target-plans", Preparation: prep, CutoverIntent: true, SourcesStopped: true, Services: []maintenance.Service{}, Participants: []maintenance.Participant{}, Blockers: []string{}}
	if e := m.store.Create(context.Background(), o); e != nil {
		t.Fatal(e)
	}
	return m, o
}
func maintenanceCapableWorkerFixture(t *testing.T, same bool) (maintenanceControl, maintenance.Operation) {
	m, o := maintenanceWorkerFixture(t, same)
	m.store.Root = t.TempDir()
	o.CreateDigest = ""
	o.SourcePackage = filepath.Join(m.store.Root, "source-package")
	o.CandidatePackage = filepath.Join(m.store.Root, "candidate-package")
	if same {
		o.CandidatePackage = o.SourcePackage
	}
	for _, root := range []string{o.SourcePackage, o.CandidatePackage} {
		writeHeldTarget(t, root, `1`)
	}
	o.Preparation, _ = json.Marshal(map[string]any{"sourcePackage": o.SourcePackage, "sourceCodex": "/source-native", "sourcePlansDigest": "source-plans", "candidatePackage": o.CandidatePackage, "candidateCodex": "/target-native", "plansDigest": "target-plans"})
	if err := m.store.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return m, o
}
func writeHeldTarget(t *testing.T, root, value string) {
	t.Helper()
	path := filepath.Join(root, "share/workspace-portal/runtime-contract.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	text := `{"maintenancePolicy":2,"maintenanceStateSchema":2,"maintenanceHeldCompletionPolicy":` + value + `}`
	if value == "absent" {
		text = `{"maintenancePolicy":1,"maintenanceMcpPolicy":1}`
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func claimWorker(t *testing.T, m maintenanceControl, invocation, selected, token string) error {
	t.Helper()
	_, e := m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "worker-claim", OperationID: controlOperationID, Invocation: invocation, Operation: &maintenance.Operation{SourcePackage: selected, SourceProfileToken: token}})
	return e
}
func TestMaintenanceRecoveryBeforeActivationRestoresSourceWithoutChangingPreparation(t *testing.T) {
	m, o := maintenanceCapableWorkerFixture(t, false)
	if e := claimWorker(t, m, "first", o.SourcePackage, "source-token"); e != nil {
		t.Fatal(e)
	}
	if e := claimWorker(t, m, "recovery", o.SourcePackage, "source-token"); e != nil {
		t.Fatal(e)
	}
	state, e := m.store.Load(controlOperationID)
	if e != nil || !state.SourceRecovered || string(state.Preparation) != string(o.Preparation) || state.CandidatePackage != o.CandidatePackage {
		t.Fatalf("%+v %v", state, e)
	}
	req := maintenance.Request{Schema: 1, Action: "activation-intent", Invocation: "recovery", OperationID: controlOperationID, Operation: &maintenance.Operation{ActivationPackage: o.CandidatePackage, ActivationNative: "/target-native", ActivationPlansDigest: "target-plans"}}
	if _, e = m.worker(context.Background(), req); e == nil {
		t.Fatal("recovery changed source target")
	}
	req.Operation = &maintenance.Operation{ActivationPackage: o.SourcePackage, ActivationNative: "/source-native", ActivationPlansDigest: "source-plans"}
	if _, e = m.worker(context.Background(), req); e != nil {
		t.Fatal(e)
	}
	state, _ = m.store.Load(controlOperationID)
	if !state.ActivationIntent || state.ActivationPackage != o.SourcePackage {
		t.Fatal(state)
	}
}
func TestMaintenanceRecoveryAfterActivationKeepsTargetEvenSameProfilePath(t *testing.T) {
	for _, same := range []bool{false, true} {
		m, o := maintenanceCapableWorkerFixture(t, same)
		if e := claimWorker(t, m, "first", o.SourcePackage, "source-token"); e != nil {
			t.Fatal(e)
		}
		_, e := m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "activation-intent", Invocation: "first", OperationID: controlOperationID, Operation: &maintenance.Operation{ActivationPackage: o.CandidatePackage, ActivationNative: "/target-native", ActivationPlansDigest: "target-plans"}})
		if e != nil {
			t.Fatal(e)
		}
		if e = claimWorker(t, m, "recovery", o.CandidatePackage, "possibly-selected"); e != nil {
			t.Fatal(e)
		}
		state, _ := m.store.Load(controlOperationID)
		if state.SourceRecovered || state.ActivationPackage != o.CandidatePackage {
			t.Fatal("target intent lost", state)
		}
	}
}
func TestMaintenanceRecoveryBeforeIntentRejectsUnprovedSelectedSource(t *testing.T) {
	m, o := maintenanceCapableWorkerFixture(t, false)
	if e := claimWorker(t, m, "first", o.SourcePackage, "source-token"); e != nil {
		t.Fatal(e)
	}
	if e := claimWorker(t, m, "recovery", "/candidate", "different-token"); e == nil {
		t.Fatal("inferred preselection from profile")
	}
}

func TestInventoryWaitsOnlyForTypedTransportLossWithExactSource(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "native.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	process, e := processIdentity(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	exe, e := os.Readlink(fmt.Sprintf("/proc/%d/exe", os.Getpid()))
	if e != nil {
		t.Fatal(e)
	}
	epoch, e := session.SocketIdentity(socket)
	if e != nil {
		t.Fatal(e)
	}
	source := maintenance.Service{PID: os.Getpid(), ProcessIdentity: process, Native: exe, Socket: socket, SocketEpoch: epoch}
	m := maintenanceControl{}
	var unavailable *inventoryUnavailable
	if e = m.inventoryFailure(source, fmt.Errorf("transport: %w", net.ErrClosed)); !errors.As(e, &unavailable) {
		t.Fatal(e)
	}
	if e = m.inventoryFailure(source, errors.New("serverShuttingDown")); errors.As(e, &unavailable) {
		t.Fatal("guessed transport from text")
	}
	source.SocketEpoch = "replacement"
	if e = m.inventoryFailure(source, net.ErrClosed); errors.As(e, &unavailable) {
		t.Fatal("replacement source treated as waiting transport")
	}
}

func TestMaintenanceTerminalRestorationRejectsRecreatedSession(t *testing.T) {
	for _, change := range []string{"none", "root", "incarnation"} {
		t.Run(change, func(t *testing.T) {
			m, _ := maintenanceWorkerFixture(t, false)
			slug := "2026-10-28-terminal"
			directory := filepath.Join(m.workspace, "work", slug)
			if e := os.MkdirAll(directory, 0700); e != nil {
				t.Fatal(e)
			}
			manifest := []byte("schema: 1\nslug: " + slug + "\ncodex:\n  thread_id: original-root\n")
			if e := os.WriteFile(filepath.Join(directory, "portal.yml"), manifest, 0600); e != nil {
				t.Fatal(e)
			}
			inc, e := trackingIncarnation(m.workspace, slug)
			if e != nil {
				t.Fatal(e)
			}
			_, e = m.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error {
				o.Services = []maintenance.Service{{Workspace: m.workspace, Unit: "unit", Native: "/native", PID: 123, ProcessIdentity: "source", Socket: "/socket", SocketEpoch: "epoch"}}
				o.TerminalIntents = []maintenance.TerminalIntent{{Workspace: m.workspace, Slug: slug, Root: "original-root", Incarnation: inc}}
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			if change == "root" {
				if e = os.WriteFile(filepath.Join(directory, "portal.yml"), []byte("schema: 1\nslug: "+slug+"\ncodex:\n  thread_id: replacement-root\n"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if change == "incarnation" {
				if e = os.Rename(directory, directory+"-retired"); e != nil {
					t.Fatal(e)
				}
				if e = os.MkdirAll(directory, 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(directory, "portal.yml"), manifest, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if e = os.WriteFile(filepath.Join(directory, "state.md"), []byte("---\nlifecycle: active\n---\n"), 0600); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(directory, "plan.md"), []byte("Fixture plan\n"), 0600); e != nil {
				t.Fatal(e)
			}
			_, e = m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "terminal-restoration", OperationID: controlOperationID})
			if (e == nil) != (change == "none") {
				t.Fatal(e)
			}
		})
	}
}

func TestMaintenanceWorkerClaimRepairsInterruptedInitialPublication(t *testing.T) {
	m, _ := maintenanceWorkerFixture(t, false)
	if e := os.Remove(filepath.Join(m.store.Root, "maintenance", "active.json")); e != nil {
		t.Fatal(e)
	}
	if _, e := m.store.Active(); e == nil || errors.Is(e, os.ErrNotExist) {
		t.Fatal("orphan ceased fencing")
	}
	if e := claimWorker(t, m, "recover", "/source", "source-token"); e != nil {
		t.Fatal(e)
	}
	active, e := m.store.Active()
	if e != nil || active.OperationID != controlOperationID {
		t.Fatalf("%+v %v", active, e)
	}
	if e = claimWorker(t, m, "recover", "/source", "source-token"); e != nil {
		t.Fatal("exact recovery retry", e)
	}
}
func TestMaintenanceWorkerClaimDoesNotReplaceForeignActiveOwner(t *testing.T) {
	m, old := maintenanceWorkerFixture(t, false)
	ctx := context.Background()
	if _, e := m.store.Update(ctx, old.OperationID, 0, func(o *maintenance.Operation) error { o.Phase = "complete"; return nil }); e != nil {
		t.Fatal(e)
	}
	other := old
	other.OperationID = "22223344-1122-4122-8122-112233441122"
	other.Phase = "prepared"
	other.CutoverIntent = false
	other.CreateDigest = ""
	if e := m.store.Create(ctx, other); e != nil {
		t.Fatal(e)
	}
	if e := claimWorker(t, m, "recover", "/source", "source-token"); e == nil {
		t.Fatal("foreign active owner replaced")
	}
	active, e := m.store.Active()
	if e != nil || active.OperationID != other.OperationID {
		t.Fatalf("%+v %v", active, e)
	}
}

func TestMaintenanceWorkerClaimRefusesMissingOrMalformedCreationDigest(t *testing.T) {
	for _, digest := range []string{"", "not-a-digest", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		m, _ := maintenanceWorkerFixture(t, false)
		if _, e := m.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error { o.CreateDigest = digest; return nil }); e != nil {
			t.Fatal(e)
		}
		if e := os.Remove(filepath.Join(m.store.Root, "maintenance", "active.json")); e != nil {
			t.Fatal(e)
		}
		if e := claimWorker(t, m, "recover", "/source", "source-token"); e == nil {
			t.Fatal("invalid creation identity repaired", digest)
		}
		if _, e := os.Stat(filepath.Join(m.store.Root, "maintenance", "active.json")); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("bad digest published pointer", e)
		}
	}
}

func TestMaintenanceScopedControlRetainsHistoricalSourceOwner(t *testing.T) {
	m, o := maintenanceWorkerFixture(t, false)
	owner := filepath.Join(m.store.Root, "retained-source")
	if err := os.MkdirAll(filepath.Join(owner, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	inputPath, argsPath := filepath.Join(m.store.Root, "request"), filepath.Join(m.store.Root, "args")
	script := fmt.Sprintf("#!/bin/sh\ncat > '%s'\nprintf '%%s\\n' \"$@\" > '%s'\nprintf '%%s\\n' '{\"schema\":1,\"status\":\"historical-owner\"}'\n", inputPath, argsPath)
	for _, name := range []string{"workspace-portal", "workspace-host"} {
		if err := os.WriteFile(filepath.Join(owner, "bin", name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.SourcePackage = owner; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, worker := range []bool{false, true} {
		raw := []byte(fmt.Sprintf("{\"schema\":1,\"action\":\"inspect\",\"operationId\":%q,\"invocation\":\"original-invocation\"}\n", o.OperationID))
		args := []string{"--user-state-root", m.store.Root, "--host-profile", m.store.Profile, "--workspace", m.workspace}
		if worker {
			args = append(args, "--worker")
		}
		var output bytes.Buffer
		if err := maintenanceControlCommand(args, bytes.NewReader(raw), &output); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(inputPath)
		argv, _ := os.ReadFile(argsPath)
		if !bytes.Equal(raw, got) || !bytes.Contains(argv, []byte("maintenance-control\n")) || bytes.Contains(argv, []byte("--worker\n")) != worker || !bytes.Contains(output.Bytes(), []byte("historical-owner")) {
			t.Fatal(string(got), string(argv), output.String())
		}
	}
	if err := os.Remove(filepath.Join(owner, "bin/workspace-host")); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenanceProtocolOwner(m.store, maintenance.Request{OperationID: o.OperationID, Action: "inspect"}); err == nil {
		t.Fatal("missing retained owner fell back to selected target")
	}
}

func TestMaintenanceHeldTargetAdmissionBeforePrepareAndActivation(t *testing.T) {
	for _, value := range []string{"absent", "0", "2", `"1"`, "1.0", "true", "null", "1"} {
		for _, sourceRecovery := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/source-recovery=%t", value, sourceRecovery), func(t *testing.T) {
				m, o := maintenanceCapableWorkerFixture(t, false)
				target, native, plans := o.CandidatePackage, "/target-native", "target-plans"
				if sourceRecovery {
					target, native, plans = o.SourcePackage, "/source-native", "source-plans"
				}
				writeHeldTarget(t, target, value)
				_, err := m.store.Update(context.Background(), o.OperationID, 0, func(current *maintenance.Operation) error { current.SourceRecovered = sourceRecovery; return nil })
				if err != nil {
					t.Fatal(err)
				}
				_, err = m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "activation-intent", OperationID: o.OperationID, Operation: &maintenance.Operation{ActivationPackage: target, ActivationNative: native, ActivationPlansDigest: plans}})
				if (err == nil) != (value == "1") {
					t.Fatal(value, sourceRecovery, err)
				}
				saved, _ := m.store.Load(o.OperationID)
				if saved.ActivationIntent != (value == "1") {
					t.Fatal("intent written without actual target capability", saved)
				}
				// Prepare uses a separate disposable store, so refusal must precede state
				// creation and observer retention, not merely later activation.
				if !sourceRecovery {
					m.store = maintenance.Store{Root: t.TempDir(), Profile: o.Profile}
					o.Phase, o.CutoverIntent = "prepared", false
					o.Services, o.Participants = []maintenance.Service{}, []maintenance.Participant{}
					_, err = m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "prepare", OperationID: o.OperationID, Operation: &o})
					if (err == nil) != (value == "1") {
						t.Fatal("prepare", err)
					}
					_, statErr := os.Stat(m.store.Path(o.OperationID))
					if (statErr == nil) != (value == "1") {
						t.Fatal("prepare refusal created journal", statErr)
					}
				}
			})
		}
	}
}
func TestMaintenanceHeldTargetStrictDescriptorAndPreparedIdentity(t *testing.T) {
	for _, body := range []string{`{"maintenanceHeldCompletionPolicy":0,"maintenanceHeldCompletionPolicy":1}`, `{"maintenanceHeldCompletionPolicy":1} trailing`, `{`, "null", strings.Repeat(" ", 64<<10+1)} {
		root := t.TempDir()
		writeHeldTarget(t, root, "1")
		path := filepath.Join(root, "share/workspace-portal/runtime-contract.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if requireMaintenanceHeldTarget(root) == nil {
			t.Fatal("accepted malformed descriptor")
		}
	}
	m, o := maintenanceCapableWorkerFixture(t, false)
	m.store = maintenance.Store{Root: t.TempDir(), Profile: o.Profile}
	o.CandidatePackage = o.SourcePackage
	if _, err := m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "prepare", OperationID: o.OperationID, Operation: &o}); err == nil {
		t.Fatal("prepared target substitution accepted")
	}
	if _, err := os.Stat(m.store.Path(o.OperationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("substitution created state", err)
	}
}

// Exercise the receiver guards before checking their wire-safe diagnostic reason.
func TestMaintenanceControlDiagnosticCompletionRefusals(t *testing.T) {
	for _, scenario := range []struct {
		name, reason string
		active       int
	}{
		{"authority", "startup-recovery-authority-changed", 0},
		{"marker", "startup-launch-marker-tuple-changed", 0},
		{"unfinished-grant", "startup-grant-has-not-reached-native-launch", 0},
		{"worker", "maintenance-worker-invocation-changed", 0},
		{"destination", "destination-identity-required", 0},
		{"destination-unit", "destination-service-changed", 1},
		{"destination-epoch", "destination-epoch-unproved", 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := startupRegistrationFixture(t, scenario.active)
			request := f.request
			request.Action = "startup-reconcile"
			switch scenario.name {
			case "authority":
				request.Startup = &maintenance.StartupSelection{AuthorizedProfileToken: "foreign"}
			case "marker":
				f.startTargets()
				f.consumeTargets()
				grant := f.grants[0]
				grant.ArgvDigest = "foreign"
				f.writeMarker(0, grant)
			case "unfinished-grant":
				request.Action = "startup-finish"
			case "worker":
				if _, err := f.control.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error { o.WorkerInvocation = "owner"; return nil }); err != nil {
					t.Fatal(err)
				}
				request.Action = "destination-ready"
				request.Invocation = "foreign"
			case "destination":
				request.Action = "destination-ready"
			case "destination-unit", "destination-epoch":
				request.Action = "destination-ready"
				request.Operation = &maintenance.Operation{Services: append([]maintenance.Service(nil), f.operation.Services...)}
				if scenario.name == "destination-unit" {
					request.Operation.Services[0].Unit = "foreign"
				}
			}
			before, err := f.control.store.Load(controlOperationID)
			if err != nil {
				t.Fatal(err)
			}
			startup, err := f.control.store.Startup()
			if err != nil {
				t.Fatal(err)
			}
			response, err := f.control.worker(context.Background(), request)
			if err == nil {
				t.Fatal("receiver admitted invalid completion", response)
			}
			if got := maintenanceControlFailureReason(err); got != scenario.reason {
				t.Errorf("reason %q, want %q (receiver: %v)", got, scenario.reason, err)
			}
			after, loadErr := f.control.store.Load(controlOperationID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			current, loadErr := f.control.store.Startup()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(startup, current) {
				t.Fatal("refusal changed operation or startup evidence")
			}
		})
	}
}

func TestMaintenanceControlDiagnosticFallbackAndTypedReasons(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{errors.New("PRIVATE-SENTINEL payload"), "identity-or-proof-refused"},
		{errors.New("startup recovery authority changed PRIVATE-SENTINEL"), "identity-or-proof-refused"},
		{fmt.Errorf("wrapped: %w", &maintenance.ProofError{Reason: "source-socket-changed"}), "source-socket-changed"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "maintenance-timeout"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "maintenance-cancelled"},
	} {
		got := maintenanceControlFailureReason(test.err)
		if got != test.want || strings.Contains(got, "PRIVATE-SENTINEL") {
			t.Fatalf("reason %q, want %q", got, test.want)
		}
	}
}

func TestMaintenanceTerminalSyncTransfersOwnedIdleRootPermission(t *testing.T) {
	f := maintenanceTerminalSyncFixture(t, "updated")
	before, err := f.recovery.Load(f.request.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if before.Active(f.request.ThreadID, f.epoch) || !before.Automatic {
		t.Fatal("fixture did not retain a generation hold")
	}
	operation, _ := f.control.store.Load(controlOperationID)
	response, err := f.control.worker(context.Background(), f.request)
	if err != nil || response.Status != "terminal-sync-admitted" {
		t.Fatalf("%+v %v", response, err)
	}
	after, err := f.recovery.Load(f.request.Slug)
	if err != nil || !after.Active(f.request.ThreadID, f.epoch) {
		t.Fatalf("admitted terminal remains held: %+v %v", after, err)
	}
	if !after.Automatic || !slices.Equal(after.ActiveThreads, []string{f.request.ThreadID}) || after.Continuation != nil {
		t.Fatal("old generation permission survived", after)
	}
	// A failed client effect leaves this same tail open; retry must not duplicate
	// permission, change completion or manufacture a native submission.
	snapshot := f.snapshot(t)
	response, err = f.control.worker(context.Background(), f.request)
	if err != nil || response.Status != "terminal-sync-admitted" {
		t.Fatalf("retry: %+v %v", response, err)
	}
	if !reflect.DeepEqual(snapshot, f.snapshot(t)) {
		t.Fatal("same-tail retry changed durable state")
	}
	current, _ := f.control.store.Load(controlOperationID)
	if !reflect.DeepEqual(operation, current) || current.Restoration.Completed || len(current.Participants) != 0 {
		t.Fatal("permission changed operation completion or participants")
	}
}

func TestMaintenanceTerminalSyncPreservesCurrentMemberAndOutcomeIdentity(t *testing.T) {
	for _, outcome := range []string{"updated", "source-recovered", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			f := maintenanceTerminalSyncFixture(t, outcome)
			if err := f.recovery.Update(context.Background(), f.request.Slug, func(r *session.Recovery) error {
				r.SocketIdentity = f.epoch
				r.ActiveThreads = []string{"member"}
				r.Continuation = &session.RecoveryContinuation{RequestID: "current-continuation"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			response, err := f.control.worker(context.Background(), f.request)
			if err != nil || response.Status != "terminal-sync-admitted" {
				t.Fatalf("%+v %v", response, err)
			}
			record, _ := f.recovery.Load(f.request.Slug)
			if !slices.Equal(record.ActiveThreads, []string{"member", f.request.ThreadID}) || !record.Automatic || record.Continuation == nil || record.Continuation.RequestID != "current-continuation" {
				t.Fatal("current member permission changed", record)
			}
		})
	}
}

func TestMaintenanceTerminalSyncRefusesForeignOrUnownedRecovery(t *testing.T) {
	for _, fault := range []string{"worker", "operation", "root", "incarnation", "manifest-root", "manifest-socket", "incomplete-provenance", "lifecycle", "socket", "source-process", "destination-process", "selected-package", "absent-restoration", "completed-restoration", "missing-recovery", "unreadable-recovery", "recovery-root", "recovery-socket", "foreign-epoch"} {
		t.Run(fault, func(t *testing.T) {
			outcome := "updated"
			if fault == "source-process" {
				outcome = "cancelled"
			}
			f := maintenanceTerminalSyncFixture(t, outcome)
			mutate := func(change func(*maintenance.Operation)) {
				_, err := f.control.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error { change(o); return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "worker":
				f.request.Invocation = "foreign"
			case "operation":
				f.request.OperationID = "22223344-1122-4122-8122-112233441122"
			case "root":
				f.request.ThreadID = "foreign"
			case "incarnation":
				mutate(func(o *maintenance.Operation) {
					o.TerminalIntents[0].Incarnation = "foreign"
					o.Restoration.Terminals[0].Incarnation = "foreign"
				})
			case "manifest-root", "manifest-socket", "incomplete-provenance":
				path := filepath.Join(f.control.workspace, "work", f.request.Slug, "portal.yml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				value := string(data)
				switch fault {
				case "manifest-root":
					value = strings.Replace(value, "thread_id: root", "thread_id: foreign", 1)
				case "manifest-socket":
					value = strings.Replace(value, "socket_path: "+f.socket, "socket_path: /foreign.sock", 1)
				case "incomplete-provenance":
					value = strings.Replace(value, "  client_version: 0.165.0\n", "", 1)
				}
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			case "lifecycle":
				locks := filepath.Join(f.control.workspace, "worktrees/.locks")
				if err := os.MkdirAll(locks, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(locks, f.request.Slug+".removal.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				if err := os.Rename(f.socket, f.socket+".replaced"); err != nil {
					t.Fatal(err)
				}
				l, err := net.Listen("unix", f.socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
			case "source-process":
				mutate(func(o *maintenance.Operation) { o.Services[0].ProcessIdentity = "foreign" })
			case "destination-process":
				mutate(func(o *maintenance.Operation) { o.Services[0].DestinationProcessIdentity = "foreign" })
			case "selected-package":
				if err := os.Remove(f.control.store.Profile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.root, f.control.store.Profile); err != nil {
					t.Fatal(err)
				}
			case "absent-restoration":
				mutate(func(o *maintenance.Operation) { o.Restoration = nil })
			case "completed-restoration":
				mutate(func(o *maintenance.Operation) {
					o.Restoration.Completed = true
					o.Phase = "complete"
					o.Outcome = "updated"
				})
			case "missing-recovery":
				if err := os.Remove(f.recoveryPath); err != nil {
					t.Fatal(err)
				}
			case "unreadable-recovery":
				if err := os.Chmod(f.recoveryPath, 0644); err != nil {
					t.Fatal(err)
				}
			default:
				if err := f.recovery.Update(context.Background(), f.request.Slug, func(r *session.Recovery) error {
					switch fault {
					case "recovery-root":
						r.ThreadID = "foreign"
					case "recovery-socket":
						r.SocketPath = "/foreign.sock"
					case "foreign-epoch":
						r.SocketIdentity = "third-generation"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := f.snapshot(t)
			if response, err := f.control.worker(context.Background(), f.request); err == nil {
				t.Fatal("unproved terminal admitted", response)
			}
			if !reflect.DeepEqual(snapshot, f.snapshot(t)) {
				t.Fatal("refusal changed durable recovery or operation state")
			}
		})
	}
}

func TestMaintenanceTerminalSyncSuppressesStoppedOrDisabledRoot(t *testing.T) {
	for _, fault := range []string{"stopped", "disabled"} {
		t.Run(fault, func(t *testing.T) {
			f := maintenanceTerminalSyncFixture(t, "updated")
			if fault == "disabled" {
				if err := f.recovery.Update(context.Background(), f.request.Slug, func(r *session.Recovery) error { r.Automatic = false; return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := f.control.store.Update(context.Background(), controlOperationID, 0, func(o *maintenance.Operation) error {
					o.Participants = []maintenance.Participant{{Workspace: f.control.workspace, Slug: f.request.Slug, Root: f.request.ThreadID, Thread: f.request.ThreadID, Incarnation: o.TerminalIntents[0].Incarnation, Cwd: filepath.Join(f.control.workspace, "work", f.request.Slug), SourceTurn: "turn", SocketEpoch: o.Services[0].SocketEpoch, NoticeID: controlOperationID, NoticeText: "pause", NoticeAction: "steer", NoticeDisposition: "accepted", ContinuationID: "22223344-1122-4122-8122-112233441122", ContinuationDisposition: "suppressed", StopRevision: 1}}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			snapshot := f.snapshot(t)
			response, err := f.control.worker(context.Background(), f.request)
			if err != nil || response.Status != "terminal-sync-suppressed" {
				t.Fatalf("%+v %v", response, err)
			}
			if !reflect.DeepEqual(snapshot, f.snapshot(t)) {
				t.Fatal("suppression changed durable permission")
			}
		})
	}
}

type maintenanceTerminalSyncTest struct {
	control                           maintenanceControl
	recovery                          session.RecoveryStore
	request                           maintenance.Request
	root, socket, epoch, recoveryPath string
}

func maintenanceTerminalSyncFixture(t *testing.T, outcome string) *maintenanceTerminalSyncTest {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "mts-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := &maintenanceTerminalSyncTest{root: root, socket: filepath.Join(root, "runtime/app-server.sock")}
	f.control = maintenanceControl{store: maintenance.Store{Root: filepath.Join(root, "state"), Profile: filepath.Join(root, "profile")}, workspace: filepath.Join(root, "workspace")}
	f.recovery = session.RecoveryStore{Root: f.control.store.Root, Workspace: f.control.workspace}
	f.request = maintenance.Request{Schema: 1, Action: "terminal-sync", OperationID: controlOperationID, Invocation: "owner", Slug: "2026-06-06-terminal", ThreadID: "root"}
	tracking := filepath.Join(f.control.workspace, "work", f.request.Slug)
	if err := os.MkdirAll(tracking, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: " + f.request.Slug + "\ncodex:\n  thread_id: root\n  socket_path: " + f.socket + "\n  client_version: 0.165.0\n"
	for name, body := range map[string]string{"portal.yml": manifest, "state.md": "---\nlifecycle: active\n---\n", "plan.md": "Fixture", "goal.txt": "retained goal"} {
		if err := os.WriteFile(filepath.Join(tracking, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A real managed terminal has complete retained Codex provenance.
	summary, err := session.Find(f.control.workspace, f.request.Slug)
	if err != nil || summary.Archived || summary.Codex.ThreadID != f.request.ThreadID || summary.Codex.SocketPath != f.socket {
		t.Fatalf("invalid retained terminal fixture: %+v %v", summary, err)
	}
	if err := os.MkdirAll(filepath.Dir(f.socket), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := net.ListenUnix("unix", &net.UnixAddr{Name: f.socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	source.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = source.Close() })
	sourceEpoch, err := session.SocketIdentity(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	process, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	native, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.recovery.Set(context.Background(), f.request.Slug, f.request.ThreadID, f.socket, true, true); err != nil {
		t.Fatal(err)
	}
	if err := f.recovery.Update(context.Background(), f.request.Slug, func(r *session.Recovery) error {
		r.ActiveThreads = append(r.ActiveThreads, "old-member")
		r.Continuation = &session.RecoveryContinuation{RequestID: "old-continuation"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.recovery.Set(context.Background(), "other-root", "unrelated", f.socket, true, false); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(f.control.store.Root, "session-recovery", "*", f.request.Slug+".json"))
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	f.recoveryPath = paths[0]
	service := maintenance.Service{Workspace: f.control.workspace, Unit: "workspace-codex@fixture.service", Socket: f.socket, SocketEpoch: sourceEpoch, PID: os.Getpid(), ProcessIdentity: process, Native: native}
	sourcePackage, target := filepath.Join(root, "source-package"), filepath.Join(root, "target-package")
	for _, p := range []string{sourcePackage, target} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selected := target
	if outcome != "updated" {
		selected = sourcePackage
	}
	if err := os.Symlink(selected, f.control.store.Profile); err != nil {
		t.Fatal(err)
	}
	inc, err := trackingIncarnation(f.control.workspace, f.request.Slug)
	if err != nil {
		t.Fatal(err)
	}
	o := maintenance.Operation{Schema: maintenance.Schema, OperationID: controlOperationID, Revision: 1, Phase: "resuming", Profile: f.control.store.Profile, SourceProfileToken: "source-token", SourcePackage: sourcePackage, CandidatePackage: target, PlansDigest: "plans", WorkerInvocation: "owner", Participants: []maintenance.Participant{}, Services: []maintenance.Service{}, TerminalIntents: []maintenance.TerminalIntent{{Workspace: f.control.workspace, Slug: f.request.Slug, Root: f.request.ThreadID, Incarnation: inc}}, ContinuationsSettled: true, NoticeCleanupComplete: true}
	if outcome == "cancelled" {
		o.CancelRequested = true
	} else {
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.socket, f.socket+".source"); err != nil {
			t.Fatal(err)
		}
		destination, err := net.Listen("unix", f.socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = destination.Close() })
		service.DestinationEpoch, err = session.SocketIdentity(f.socket)
		if err != nil {
			t.Fatal(err)
		}
		service.DestinationPID, service.DestinationProcessIdentity, service.DestinationNative = os.Getpid(), process, native
		o.SourcesStopped, o.CutoverIntent, o.ActivationIntent = true, true, true
		o.ActivationPackage, o.ActivationNative, o.ActivationPlansDigest = selected, native, "plans"
		o.SourceRecovered = outcome == "source-recovered"
	}
	f.epoch, err = session.SocketIdentity(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]any{"launch": map[string]string{"codex_path": native}})
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.socket), "registration.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	o.Services = []maintenance.Service{service}
	if err := f.control.store.Create(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	controller := maintenance.Controller{Store: f.control.store, WorkerInvocation: "owner"}
	if _, err := controller.BeginRestoration(context.Background(), controlOperationID, maintenance.RestorationIntent{Outcome: outcome, Package: selected, ProfileToken: "source-token"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *maintenanceTerminalSyncTest) snapshot(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(f.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || strings.HasSuffix(path, ".lock") {
			return nil
		}
		b, err := os.ReadFile(path)
		files[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func maintenancePrepareFixture(t *testing.T) (maintenanceControl, maintenance.Operation) {
	t.Helper()
	m, o := maintenanceCapableWorkerFixture(t, false)
	m.store.Root = t.TempDir()
	o.Phase, o.CutoverIntent, o.SourcesStopped = "prepared", false, false
	return m, o
}

func requireNoMaintenancePreparation(t *testing.T, m maintenanceControl, id string) {
	t.Helper()
	for _, path := range []string{m.store.Path(id), filepath.Join(m.store.Root, "maintenance", "active.json"), filepath.Join(m.store.Root, "maintenance", "startup.json")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected preparation wrote %s: %v", path, err)
		}
	}
}

func TestMaintenancePrepareDiagnosticRefusalsBeforeCreate(t *testing.T) {
	for _, scenario := range []struct{ name, reason string }{
		{"identity", "prepare-operation-identity-changed"},
		{"target", "prepare-target-changed"},
		{"descriptor", "prepare-target-validation-failed"},
		{"process", "prepare-source-process-unavailable"},
		{"executable", "prepare-source-native-unavailable"},
		{"socket", "prepare-source-socket-unavailable"},
		{"create", "prepare-create-failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, o := maintenancePrepareFixture(t)
			request := maintenance.Request{Schema: 1, Action: "prepare", OperationID: o.OperationID, Operation: &o}
			switch scenario.name {
			case "identity":
				request.OperationID = "22223344-1122-4122-8122-112233441122"
			case "target":
				o.CandidatePackage = "/PRIVATE-SENTINEL"
			case "descriptor":
				if err := os.WriteFile(filepath.Join(o.CandidatePackage, "share/workspace-portal/runtime-contract.json"), []byte("PRIVATE-SENTINEL"), 0600); err != nil {
					t.Fatal(err)
				}
			case "process":
				o.Services = []maintenance.Service{{PID: -1}}
			case "executable":
				// An exited child not yet waited for still has /proc/stat, but no exe.
				command := exec.Command("sh", "-c", "exit 0")
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				defer command.Wait()
				var info unix.Siginfo
				if err := unix.Waitid(unix.P_PID, command.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
					t.Fatal(err)
				}
				o.Services = []maintenance.Service{{PID: command.Process.Pid}}
			case "socket":
				o.Services = []maintenance.Service{{PID: os.Getpid(), Socket: filepath.Join(m.store.Root, "PRIVATE-SENTINEL.sock")}}
			case "create":
				o.SourceProfileToken = "" // Existing Store.Create identity validation.
			}
			_, err := m.worker(context.Background(), request)
			if err == nil || maintenanceControlFailureReason(err) != scenario.reason {
				t.Fatalf("reason %q, want %q: %v", maintenanceControlFailureReason(err), scenario.reason, err)
			}
			requireNoMaintenancePreparation(t, m, o.OperationID)
		})
	}
}

func TestMaintenancePrepareDiagnosticFallbackPreservesTypedCauses(t *testing.T) {
	for _, stage := range []string{"prepare-target-validation-failed", "prepare-source-process-unavailable", "prepare-source-native-unavailable", "prepare-source-socket-unavailable", "prepare-inventory-failed", "prepare-inventory-metadata-unavailable", "prepare-inventory-native-proof-unavailable", "prepare-create-failed"} {
		for _, test := range []struct {
			err  error
			want string
		}{
			{errors.New("PRIVATE-SENTINEL /peer/session native payload"), stage},
			{errors.New("ambiguous retained root ownership PRIVATE-SENTINEL"), stage},
			{&maintenance.ProofError{Reason: "source-socket-changed"}, "source-socket-changed"},
			{context.Canceled, "maintenance-cancelled"},
			{context.DeadlineExceeded, "maintenance-timeout"},
			{&inventoryUnavailable{context.DeadlineExceeded}, "maintenance-timeout"},
			{&inventoryUnavailable{context.Canceled}, "maintenance-cancelled"},
			{&inventoryUnavailable{&maintenance.ProofError{Reason: "source-process-changed"}}, "source-process-changed"},
		} {
			err := maintenancePrepareFailure(stage, fmt.Errorf("PRIVATE-SENTINEL: %w", test.err))
			if got := maintenanceControlFailureReason(err); got != test.want || !errors.Is(err, test.err) {
				t.Fatalf("reason %q, want %q; cause lost=%t", got, test.want, !errors.Is(err, test.err))
			}
		}
	}
}

func TestMaintenancePreparePreservesSuccessfulAndPartialCreation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			m, o := maintenancePrepareFixture(t)
			if partial {
				// The operation directory remains writable, while publishing active.json
				// fails after the journal has been saved. No timing-dependent injection.
				directory := filepath.Join(m.store.Root, "maintenance")
				if err := os.MkdirAll(filepath.Dir(m.store.Path(o.OperationID)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "operation.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(directory, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(directory, 0700)
			}
			response, err := m.worker(context.Background(), maintenance.Request{Schema: 1, Action: "prepare", OperationID: o.OperationID, Operation: &o})
			if partial {
				if err == nil || maintenanceControlFailureReason(err) != "prepare-create-failed" {
					t.Fatalf("%+v %v", response, err)
				}
				if _, err := m.store.Active(); err == nil || errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial journal stopped fencing", err)
				}
			} else if err != nil || response.Status != "prepared" || response.Operation == nil || response.Operation.Preparation != nil {
				t.Fatalf("successful preparation changed: %+v %v", response, err)
			}
			saved, err := m.store.Load(o.OperationID)
			if err != nil || saved.Phase != "prepared" || saved.CreateDigest == "" || string(saved.Preparation) != string(o.Preparation) || saved.CandidatePackage != o.CandidatePackage {
				t.Fatalf("original creation evidence lost: %+v %v", saved, err)
			}
			if partial {
				if err := os.Chmod(filepath.Join(m.store.Root, "maintenance"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := m.store.RecoverPointer(context.Background(), o.OperationID); err != nil {
					t.Fatal(err)
				}
			}
			active, err := m.store.Active()
			if err != nil || active.OperationID != o.OperationID {
				t.Fatal(active, err)
			}
			if _, err := os.Stat(filepath.Join(m.store.Root, "maintenance", "startup.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("prepare wrote startup state", err)
			}
		})
	}
}
