package piruntime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// This is the existing finite Unix supervisor harness with authoritative fake
// H0 terminal receipts, not another transport parser or production constructor.
type successorFixtureHost struct {
	base       *fakeHost
	terminal   map[string]HostObservation
	starts     int
	failStart  bool
	lockNested bool
}

func (h *successorFixtureHost) probeLock(ref RuntimeRef) error {
	file, err := os.OpenFile(filepath.Join(h.base.m.instance(ref), "launch.lock"), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		h.lockNested = true
		return err
	}
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
func (h *successorFixtureHost) Start(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	h.starts++
	if err := h.probeLock(ref); err != nil {
		return HostObservation{}, err
	}
	if h.failStart {
		return HostObservation{}, ErrUnavailable
	}
	return h.base.Start(ctx, ref)
}
func (h *successorFixtureHost) Inspect(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	if err := h.probeLock(ref); err != nil {
		return HostObservation{}, err
	}
	if observed, ok := h.terminal[ref.RequestID]; ok {
		return observed, nil
	}
	return h.base.Inspect(ctx, ref)
}
func (h *successorFixtureHost) finish(t *testing.T, ref RuntimeRef) {
	t.Helper()
	observed, err := h.base.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	observed.Phase, observed.Outcome, observed.Result = "terminal", "stopped", "exited"
	observed.LoadState, observed.ActiveState, observed.SubState, observed.Job = "loaded", "inactive", "dead", ""
	observed.DrainProof = map[string]any{"empty": true, "boot_id": observed.BootID, "invocation_id": observed.InvocationID, "control_group": observed.ControlGroup, "load_state": "loaded", "active_state": "inactive", "job": ""}
	h.terminal[ref.RequestID] = observed
	// Simulate only this owned fixture supervisor's completed exit. Production
	// succession never stops a unit/worker or removes a session here.
	if h.base.listener != nil {
		_ = h.base.listener.Close()
	}
	if err := os.Remove(filepath.Join(h.base.m.instance(ref), "control.sock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func successorFixture(t *testing.T) (*Manager, *successorFixtureHost, StartRequest, RuntimeStatus, context.Context) {
	t.Helper()
	m, h, start, ctx := supervisorFixture(t)
	wrapper := &successorFixtureHost{base: h, terminal: map[string]HostObservation{}}
	m.config.Host = wrapper
	// Owned fake H0/package proof, never native host boot/cgroup/model access.
	m.config.verifyDrain = func(HostObservation) error { return nil }
	m.config.validatePackage = func(string) error { return nil }
	runtime, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	return m, wrapper, start, runtime, ctx
}
func nextStart(previous StartRequest, id string) StartRequest {
	next := previous
	next.RequestID = id
	next.ReplacesRequestID = previous.RequestID
	next.BindingRevision++
	next.ProfileGeneration = "successor-profile"
	next.AppStorePath = "/nix/store/11111111111111111111111111111111-successor-fixture"
	return next
}

func TestExplicitSuccessorPreservesHistoryAndNeverDispatchesHistoricalSocket(t *testing.T) {
	m, h, start, first, ctx := successorFixture(t)
	h.finish(t, first.Runtime)
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	second, err := m.Ensure(ctx, next)
	if err != nil || second.Phase != "ready" || second.Runtime.InstanceID != first.Runtime.InstanceID || second.Runtime.Generation == first.Runtime.Generation || h.starts != 2 || h.lockNested {
		t.Fatal(second, err, h.starts, h.lockNested)
	}
	records, err := m.records(start.Session)
	if err != nil || records.Schema != 2 || len(records.Requests) != 2 || records.CurrentRequestID != next.RequestID || records.Requests[start.RequestID].Start != start {
		t.Fatal(records, err)
	}
	for _, original := range []StartRequest{start, next} {
		root := filepath.Join(m.instance(first.Runtime), "requests", original.RequestID)
		if target, err := os.Readlink(filepath.Join(root, "package-root")); err != nil || target != original.AppStorePath {
			t.Fatal("per-request closure root", target, err)
		}
		if _, err := readPrivateBytes(filepath.Join(root, "launch.json"), 65536); err != nil {
			t.Fatal(err)
		}
	}
	if root, err := os.Readlink(filepath.Join(m.instance(first.Runtime), "package-root")); err != nil || root != start.AppStorePath {
		t.Fatal("legacy root retargeted", root, err)
	}
	if saved, err := m.ReadStart(ctx, start.Session); err != nil || saved != next {
		t.Fatal(saved, err)
	}
	originalDial := m.config.Dial
	m.config.Dial = func(context.Context, string) (net.Conn, error) { return nil, ErrScope }
	if historical, err := m.Ensure(ctx, start); err != nil || historical.Runtime != first.Runtime || historical.Phase != "stopped" || h.starts != 2 {
		t.Fatal("historical Ensure dispatched successor", historical, err)
	}
	if historical, err := m.Inspect(ctx, first.Runtime); err != nil || historical.Phase != "stopped" {
		t.Fatal(historical, err)
	}
	m.config.Dial = originalDial
	if replay, err := m.Ensure(ctx, next); err != nil || replay.Runtime != second.Runtime || h.starts != 2 {
		t.Fatal(replay, err)
	}
}

func TestExplicitSuccessorUnknownStartRetainsCurrentRequestAndGeneration(t *testing.T) {
	m, h, start, first, ctx := successorFixture(t)
	h.finish(t, first.Runtime)
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	h.failStart = true
	if _, err := m.Ensure(ctx, next); err == nil {
		t.Fatal("missing native uncertainty")
	}
	records, err := m.records(start.Session)
	if err != nil {
		t.Fatal(err)
	}
	reserved := records.Requests[next.RequestID].Runtime
	if records.CurrentRequestID != next.RequestID || reserved.Generation == "" || h.starts != 2 {
		t.Fatal(records, h.starts)
	}
	if _, err := m.Ensure(ctx, next); err == nil || h.starts != 2 {
		t.Fatal("unknown native start repeated", err, h.starts)
	}
	changed := next
	changed.ProfileGeneration = "changed"
	if _, err := m.Ensure(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("changed request reused", err)
	}
	escape := nextStart(next, "33333333-3333-4333-8333-333333333333")
	if _, err := m.Ensure(ctx, escape); !errors.Is(err, ErrHeld) {
		t.Fatal("unknown predecessor replaced", err)
	}
	after, err := m.records(start.Session)
	if err != nil || after.CurrentRequestID != next.RequestID || after.Requests[next.RequestID].Runtime != reserved || h.starts != 2 {
		t.Fatal(after, err, h.starts)
	}
}

func TestExplicitSuccessorPreparationRetainsGenerationAndRefusesDescriptorRepair(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "retention-interrupted", true: "descriptor-conflict"}[foreign], func(t *testing.T) {
			m, h, start, first, ctx := successorFixture(t)
			h.finish(t, first.Runtime)
			next := nextStart(start, "22222222-2222-4222-8222-222222222222")
			descriptorPath := filepath.Join(m.instance(first.Runtime), "launch.json")
			before, err := os.ReadFile(descriptorPath)
			if err != nil {
				t.Fatal(err)
			}
			m.config.retainPackage = func(_ context.Context, path, _ string) error {
				if strings.Contains(path, next.RequestID) {
					if foreign {
						return writePrivateBytes(descriptorPath, []byte("{}\n"))
					}
					return ErrUnavailable
				}
				return nil
			}
			if _, err := m.Ensure(ctx, next); err == nil {
				t.Fatal("missing preparation fault")
			}
			records, err := m.records(start.Session)
			if err != nil {
				t.Fatal(err)
			}
			reserved := records.Requests[next.RequestID]
			if reserved.Runtime.Generation == "" || reserved.Phase != "preparing" || records.CurrentRequestID != next.RequestID || h.starts != 1 {
				t.Fatal(records, h.starts)
			}
			m.config.retainPackage = func(context.Context, string, string) error { return nil }
			if foreign {
				if _, err := m.Ensure(ctx, next); !errors.Is(err, ErrConflict) {
					t.Fatal("foreign descriptor repaired", err)
				}
				if data, err := os.ReadFile(descriptorPath); err != nil || string(data) != "{}\n" || h.starts != 1 {
					t.Fatal("descriptor rollback", string(data), err, h.starts)
				}
			} else {
				if data, err := os.ReadFile(descriptorPath); err != nil || string(data) != string(before) {
					t.Fatal("publication before retained closure", err)
				}
				result, err := m.Ensure(ctx, next)
				if err != nil || result.Runtime != reserved.Runtime || h.starts != 2 {
					t.Fatal("prepared generation replaced", result, err, h.starts)
				}
			}
		})
	}
}

func TestExplicitSuccessorReloadsOnlyPositivelyUnloadedNativeHistory(t *testing.T) {
	m, h, start, first, ctx := successorFixture(t)
	creation := strings.Repeat("c", 64)
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: creation, Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	old := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	input := SendInput{Message: "original input survives explicit successor", ActionContext: "initial"}
	ref := SubmissionRef{Conversation: state.Conversation, OperationID: strings.Repeat("e", 32), Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, settingsFixture())
	request := backend.SendRequest{Message: input.Message, ActionContext: input.ActionContext, ClientUserMessageID: ref.OperationID}
	if result, err := m.Deliver(ctx, old, request); err != nil || result.Outcome != backend.Accepted {
		t.Fatal(result, err)
	}
	waitIdle(t, m, ctx, old)
	if stop, err := m.Unload(ctx, old); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
	original, err := m.Lookup(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	h.finish(t, first.Runtime)
	second, err := m.Ensure(ctx, nextStart(start, "22222222-2222-4222-8222-222222222222"))
	if err != nil {
		t.Fatal(err)
	}
	current := agentbackend.ExecutionHandle{Conversation: old.Conversation, Generation: second.Runtime.Generation}
	if _, err := m.Bind(ctx, old); !errors.Is(err, ErrScope) {
		t.Fatal("old generation dispatched", err)
	}
	loaded, err := m.Load(ctx, current)
	if err != nil || loaded.Conversation != state.Conversation || !loaded.Persisted {
		t.Fatal(loaded, err)
	}
	proof, err := m.VerifyCreation(ctx, current, creation, state.HistoryRef, &ref)
	if err != nil || proof.Generation != current.Generation || proof.Bootstrap == nil || proof.Bootstrap.Generation != old.Generation {
		t.Fatal("original creation receipt lost", proof, err)
	}
	if result, err := m.Deliver(ctx, current, request); err != nil || result.Outcome != backend.Accepted {
		t.Fatal("duplicate original receipt", result, err)
	}
	ops, err := m.Bind(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := ops.Read.Read(ctx, BackendIdentity(current))
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, entry := range transcript.Entries {
		if entry.Kind == "userMessage" {
			users++
		}
	}
	if users != 1 {
		t.Fatal("successor replayed original input", users)
	}
	after, err := m.Lookup(ctx, ref)
	if err != nil || !equalJSON(original, after) {
		t.Fatal("original receipt rewritten", err)
	}
	if stop, err := m.Unload(ctx, current); err != nil || !stop.VerifiedExit {
		t.Fatal(stop, err)
	}
}

func TestSuccessorCurrentDrainDistinguishesEmptyAbsentAndAmbiguous(t *testing.T) {
	root := t.TempDir()
	boot := filepath.Join(root, "boot")
	groups := filepath.Join(root, "cgroups")
	if err := os.Mkdir(groups, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(boot, []byte("current-boot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(groups, "cgroup.controllers"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	observed := HostObservation{BootID: "current-boot", ControlGroup: "/owned"}
	if err := verifyCurrentDrainAt(observed, boot, groups); err != nil {
		t.Fatal("proved absent group", err)
	}
	group := filepath.Join(groups, "owned")
	if err := os.Mkdir(group, 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyCurrentDrainAt(observed, boot, groups); !errors.Is(err, ErrHeld) {
		t.Fatal("existing group without events accepted as absent", err)
	}
	for _, value := range []string{"populated 0\n", "populated 1\n", "unknown 0\n", "populated 0\npopulated 0\n"} {
		if err := os.WriteFile(filepath.Join(group, "cgroup.events"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		err := verifyCurrentDrainAt(observed, boot, groups)
		if (err == nil) != (value == "populated 0\n") {
			t.Fatal(value, err)
		}
	}
	observed.BootID = "old-boot"
	if err := verifyCurrentDrainAt(observed, boot, groups); !errors.Is(err, ErrHeld) {
		t.Fatal("changed boot accepted", err)
	}
	observed.BootID = "current-boot"
	if err := os.Remove(filepath.Join(groups, "cgroup.controllers")); err != nil {
		t.Fatal(err)
	}
	if err := verifyCurrentDrainAt(observed, boot, groups); !errors.Is(err, ErrHeld) {
		t.Fatal("missing cgroup filesystem became absence", err)
	}
}

func TestExplicitSuccessorPreconditionsRejectWithoutNativeOrRecordChanges(t *testing.T) {
	for _, fault := range []string{"running", "rejected", "missing-drain", "boot-cgroup", "package", "loaded-member", "pending-settings", "unknown-input"} {
		t.Run(fault, func(t *testing.T) {
			m, h, start, first, ctx := successorFixture(t)
			h.finish(t, first.Runtime)
			directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
			switch fault {
			case "running":
				observed := h.terminal[start.RequestID]
				observed.Outcome = "running"
				h.terminal[start.RequestID] = observed
			case "missing-drain":
				observed := h.terminal[start.RequestID]
				observed.DrainProof = nil
				h.terminal[start.RequestID] = observed
			case "rejected":
				observed := h.terminal[start.RequestID]
				observed.Outcome = "rejected"
				h.terminal[start.RequestID] = observed
			case "package":
				m.config.validatePackage = func(string) error { return ErrUnavailable }
			case "boot-cgroup":
				m.config.verifyDrain = func(HostObservation) error { return ErrHeld }
			default:
				ref := agentbackend.ConversationRef{Session: start.Session, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "44444444-4444-4444-8444-444444444444"}
				root := filepath.Join(directory, "members", ref.Member, ref.Incarnation)
				settings := settingsFixture()
				record := conversationRecord{Schema: 1, Conversation: ref, HistoryRef: historyReference(ref), CreationID: strings.Repeat("c", 64), Generation: first.Runtime.Generation, Settings: settings, InitialSettings: settings}
				if fault == "loaded-member" {
					record.Loaded = true
				}
				if fault == "pending-settings" {
					target := settings
					target.Revision++
					record.PendingSettings = &target
				}
				if err := writePrivate(filepath.Join(root, "conversation.json"), record); err != nil {
					t.Fatal(err)
				}
				if fault == "unknown-input" {
					input := SendInput{Message: "unresolved original"}
					submission := SubmissionRef{Conversation: ref, OperationID: strings.Repeat("e", 32), Action: "send"}
					submission.Digest = SubmissionDigest(submission, input, settings)
					if err := (&receiptEngine{root: filepath.Join(root, "submissions")}).save(SubmissionStatus{Schema: 1, Ref: submission, Phase: "Unknown", Generation: first.Runtime.Generation, Settings: settings, Input: input}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadFile(filepath.Join(directory, "runtime.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Ensure(ctx, nextStart(start, "22222222-2222-4222-8222-222222222222")); err == nil {
				t.Fatal("unsettled predecessor replaced", fault)
			}
			after, err := os.ReadFile(filepath.Join(directory, "runtime.json"))
			if err != nil || string(after) != string(before) || h.starts != 1 {
				t.Fatal("preflight changed state", fault, err, h.starts)
			}
		})
	}
}
