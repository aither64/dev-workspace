package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// The Ruby host fixture invokes this copied test executable as the candidate
// helper. Admission, profile identity and both borrowed descriptors are real;
// only the terminal Manager port uses the existing typed fixture.
func init() {
	if os.Getenv("PI_DELETE_RECOVERY_CLI_CHILD") != "1" {
		return
	}
	if len(os.Args) >= 3 && os.Args[1] == "uploads" && os.Args[2] == "remove-session" {
		os.Exit(0)
	}
	if len(os.Args) < 3 || os.Args[1] != "pi-runtime" || os.Args[2] != "lifecycle" {
		fmt.Fprintln(os.Stderr, "unexpected recovery fixture operation")
		os.Exit(1)
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	var capture session.PiLifecycleCapture
	if err != nil || json.Unmarshal(data, &capture) != nil || capture.Terminal == nil {
		os.Exit(1)
	}
	// Enter at the admission boundary: package metadata validation is unchanged
	// and requires immutable Nix outputs, while this fixture uses disposable roots.
	values := map[string]string{}
	for i := 3; i+1 < len(os.Args); i += 2 {
		values[os.Args[i]] = os.Args[i+1]
	}
	config := Config{Workspace: values["--workspace"], WorkspaceName: values["--workspace-name"],
		UserStateRoot: values["--user-state-root"], AuthorityDir: values["--authority-dir"],
		HostProfile: values["--host-profile"], TransitionLock: values["--transition-lock"]}
	parent, err := piLifecycleParent(config, values["--slug"])
	if err == nil {
		defer parent.Close()
		var profile hostProfileIdentity
		profile, err = readHostProfileIdentity(config.HostProfile)
		if err == nil {
			config.PackageRoot, err = filepath.EvalSymlinks(config.HostProfile)
		}
		if err == nil {
			capture, err = session.DecodePiLifecycleCapture(data, config.Workspace, values["--slug"])
		}
		if err == nil {
			runtime := &piLifecycleFixture{piCreationRuntimeFixture: &piCreationRuntimeFixture{runtime: capture.Runtime}}
			config.PiCreation = &PiCreationServices{Runtime: runtime}
			s := &Server{config: config, hostProfile: profile}
			var executable string
			executable, err = os.Executable()
			if err == nil {
				var recheck func() error
				recheck, err = s.piDeleteRecoveryInvocation(values["--action"], values["--slug"], executable, data, capture)
				if err == nil {
					var result session.PiLifecycleCapture
					result, err = s.piLifecycleOperation(context.WithValue(context.Background(), piDeleteRecoveryKey{}, recheck), values["--action"], values["--slug"], capture)
					if err == nil {
						err = json.NewEncoder(os.Stdout).Encode(result)
					}
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func (f *piLifecycleFixture) VerifyLifecycleTerminal(_ context.Context, ref piruntime.RuntimeRef, saved piruntime.HostObservation) error {
	if ref != f.runtime || saved.InstanceID != ref.InstanceID || saved.Generation != ref.Generation || saved.Phase != "terminal" || saved.Outcome != "stopped" || saved.DrainProof["empty"] != true {
		return piruntime.ErrHeld
	}
	return nil
}

func TestPiDeleteRecoveryCandidateRubyGoAdmission(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, _ := filepath.Abs("../../../test/workspace_host/archive_recovery_test.rb")
	command := exec.Command("ruby", source, "--name", "/\\AWorkspaceHostTest#test_terminal_pi_delete_candidate_uses_real_ruby_go_admission\\z/")
	command.Env = append(os.Environ(), "PI_DELETE_RECOVERY_GO_FIXTURE="+binary)
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("0 failures, 0 errors, 0 skips")) {
		t.Fatalf("ordinary candidate -> Ruby -> Go admission: %v\n%s", err, output)
	}
}

// The existing creation fixture owns original reservation/native provenance;
// only the new synchronous lifecycle port is supplied here. Manager's shared
// worker fixture separately owns actual Unload/Stop/drain/Load evidence.
type piLifecycleFixture struct {
	*piCreationRuntimeFixture
	checks, interrupts, retires int
	held                        bool
	callbacks                   []piruntime.LifecycleCallback
	memberError                 error
	creationID                  string
}

func (f *piLifecycleFixture) CaptureLifecycleMembers(_ context.Context, r piruntime.RuntimeRef, slots []piruntime.LifecycleSlot) ([]piruntime.LifecycleMember, error) {
	if r != f.runtime {
		return nil, piruntime.ErrScope
	}
	result := []piruntime.LifecycleMember{}
	for _, slot := range slots {
		m := piruntime.LifecycleMember{Slot: slot}
		if slot.Member == f.state.Conversation.Member {
			if f.creationID != "" && (slot.CreationID != f.creationID || slot.InitialSettings != f.state.Settings || slot.Incarnation != f.state.Conversation.Incarnation || r.Generation != f.state.Generation) {
				return nil, piruntime.ErrScope
			}
			ref, settings := f.state.Conversation, f.state.Settings
			m.Conversation = &ref
			m.Settings = &settings
			m.Generation = r.Generation
			m.HistoryRef = f.state.HistoryRef
		}
		result = append(result, m)
	}
	return result, nil
}
func (f *piLifecycleFixture) CheckLifecycle(_ context.Context, r piruntime.RuntimeRef, m []agentbackend.ConversationRef) error {
	f.checks++
	if r != f.runtime || len(m) != 1 || m[0] != f.state.Conversation {
		return piruntime.ErrScope
	}
	if f.held {
		return piruntime.ErrHeld
	}
	return nil
}
func (f *piLifecycleFixture) InterruptLifecycle(ctx context.Context, r piruntime.RuntimeRef, m []agentbackend.ConversationRef, _ func(context.Context, piruntime.RuntimeRef, piruntime.HostCallbackConnector) error) error {
	f.interrupts++
	return f.CheckLifecycle(ctx, r, m)
}
func (f *piLifecycleFixture) Retire(_ context.Context, r piruntime.RuntimeRef, m []agentbackend.ConversationRef, _ func(context.Context, piruntime.RuntimeRef, piruntime.HostCallbackConnector) error) (piruntime.RuntimeStatus, error) {
	f.retires++
	if r != f.runtime || len(m) != 1 || m[0] != f.state.Conversation {
		return piruntime.RuntimeStatus{}, piruntime.ErrScope
	}
	return piruntime.RuntimeStatus{Runtime: r, Phase: "stopped", Observation: piruntime.HostObservation{LaunchRequest: piruntime.LaunchRequest{InstanceID: r.InstanceID, RequestID: r.RequestID, Generation: r.Generation}, Digest: r.Digest, Phase: "terminal", Outcome: "stopped", DrainProof: map[string]any{"empty": true}}}, nil
}
func (*piLifecycleFixture) Upgrade(context.Context, piruntime.UpgradeRequest, piruntime.UpgradeHooks) (piruntime.UpgradeResult, error) {
	return piruntime.UpgradeResult{}, piruntime.ErrUnavailable
}
func (f *piLifecycleFixture) TerminalCallbacks(_ context.Context, r piruntime.RuntimeRef, refs []agentbackend.ConversationRef, _ bool) ([]piruntime.LifecycleCallback, error) {
	if r != f.runtime || len(refs) != 1 || refs[0] != f.state.Conversation || len(f.callbacks) == 0 {
		return nil, piruntime.ErrHeld
	}
	return f.callbacks, nil
}
func (f *piLifecycleFixture) RetireTerminalCallbacks(ctx context.Context, r piruntime.RuntimeRef, refs []agentbackend.ConversationRef, saved []piruntime.LifecycleCallback) (piruntime.RuntimeStatus, error) {
	if !reflect.DeepEqual(saved, f.callbacks) {
		return piruntime.RuntimeStatus{}, piruntime.ErrHeld
	}
	return f.Retire(ctx, r, refs, nil)
}
func (f *piLifecycleFixture) ReadInitialStart(_ context.Context, ref agentbackend.SessionRef, id string) (piruntime.StartRequest, piruntime.RuntimeRef, error) {
	if f.start == nil || f.start.Session != ref || f.start.RequestID != id {
		return piruntime.StartRequest{}, piruntime.RuntimeRef{}, piruntime.ErrScope
	}
	return *f.start, f.runtime, nil
}
func (f *piLifecycleFixture) ReadMemberAdmission(_ context.Context, ref agentbackend.ConversationRef, creation, input string) (piruntime.ConversationState, piruntime.SubmissionStatus, error) {
	if f.memberError != nil {
		return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, f.memberError
	}
	if ref != f.state.Conversation || creation != f.creationID || f.submission == nil || input != f.submission.Ref.OperationID {
		return piruntime.ConversationState{}, piruntime.SubmissionStatus{}, piruntime.ErrScope
	}
	state := f.state
	state.Persisted = f.submission.Phase == "Accepted"
	return state, *f.submission, nil
}

func TestPiLifecycleCaptureUsesOriginalReservationsBeforeRetire(t *testing.T) {
	s, receipt, base := newPiCreationFixtureWithPreset(t, "example", historicalPiTeamPreset())
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	f := &piLifecycleFixture{piCreationRuntimeFixture: base}
	s.config.PiCreation.Runtime = f
	capture, err := s.capturePiLifecycle(context.Background(), receipt.Request.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if capture.Ref() != f.state.Conversation.Session || len(capture.Members) != len(receipt.Pi.Conversations) || len(capture.Selectors) < 5 || capture.Members[0].Slot.CreationID != receipt.ReceiptID {
		t.Fatal("lost original capture", capture)
	}
	creates, sends := f.creates, f.sends
	t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "archive")
	result, err := s.piLifecycleOperation(context.Background(), "archive-retire", receipt.Request.Slug, capture)
	if err != nil || result.Terminal == nil || f.interrupts != 1 || f.retires != 1 || f.creates != creates || f.sends != sends {
		t.Fatal(result, err, f.interrupts, f.retires)
	}
	bad := capture
	bad.Callbacks = []piruntime.LifecycleCallback{{Conversation: f.state.Conversation, Input: piruntime.SubmissionRef{Conversation: f.state.Conversation, Action: "send", OperationID: receipt.Pi.InitialInputID, Digest: strings.Repeat("e", 64)}, Origin: piruntime.NativeOriginRef{AnchorEntryID: "original", NativeToolCallID: "native-call", SelectionToken: "linear-v1:" + strings.Repeat("b", 64), Tool: "team_message"}, ArgumentsSHA256: strings.Repeat("f", 64), MessageID: receipt.Pi.InitialInputID, To: "foreign0"}}
	t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "delete")
	if _, err := s.piLifecycleOperation(context.Background(), "terminal-delete", receipt.Request.Slug, bad); err == nil || f.retires != 1 {
		t.Fatal("foreign recipient admitted", err)
	}
	bad.Callbacks[0].To = "lead"
	bad.Callbacks[0].Origin.Tool = "bash"
	if _, err := s.piLifecycleOperation(context.Background(), "terminal-delete", receipt.Request.Slug, bad); err == nil || f.retires != 1 {
		t.Fatal("shell callback admitted", err)
	}
}

func TestPiLifecycleForceDeleteCapturesMaterializedCreatingRoot(t *testing.T) {
	for _, kind := range []string{"exact", "native-result-absent", "ordinary-capture", "missing-companion", "mismatched-companion", "missing-receipt", "mismatched-receipt", "unvalidated-receipt", "wrong-native-generation", "wrong-native-result", "missing-mapping", "mismatched-mapping", "unknown-input", "missing-witness", "unexpected-completion"} {
		t.Run(kind, func(t *testing.T) {
			s, receipt, base := newPiCreationFixtureWithPreset(t, "example", historicalPiTeamPreset())
			interrupted := errors.New("interrupted before ready publication")
			s.piCreationCheckpoint = func(point string) error {
				if point == "initial_input" {
					return interrupted
				}
				return nil
			}
			if err := s.initializePiCreation(receipt); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			f := &piLifecycleFixture{piCreationRuntimeFixture: base, held: true, creationID: receipt.ReceiptID}
			s.config.PiCreation.Runtime = f
			b, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load("example")
			if err != nil || b.State != "creating" || b.Conversations[0].State != "creating" || b.Conversations[0].NativeID != "" {
				t.Fatal(b, err)
			}
			path := piJournalPath(s.config.Workspace, "example")
			j, err := readPiCreationJournal(path)
			if err != nil || j.State != "creating" || j.Native == nil || f.submission == nil || f.submission.Phase != "Accepted" {
				t.Fatal(j, err)
			}
			f.callbacks = []piruntime.LifecycleCallback{{Conversation: f.state.Conversation, Input: f.submission.Ref, Origin: piruntime.NativeOriginRef{AnchorEntryID: "original-callback", NativeToolCallID: "original-call", SelectionToken: "linear-v1:" + strings.Repeat("b", 64), Tool: "team_message"}, ArgumentsSHA256: strings.Repeat("c", 64), MessageID: "22222222-2222-4222-8222-222222222222", To: "implementer0"}}
			companion := s.creationEvidencePath(receipt) + ".request"
			switch kind {
			case "native-result-absent":
				j.Native = nil
			case "missing-companion":
				if err := os.Remove(companion); err != nil {
					t.Fatal(err)
				}
			case "mismatched-companion":
				i := j.PiCreationIntent
				i.GoalSHA256 = strings.Repeat("f", 64)
				if err := writePiCreationRecord(companion, i, i.RecordLimit()); err != nil {
					t.Fatal(err)
				}
			case "missing-receipt":
				if err := os.Remove(s.creationPath("example")); err != nil {
					t.Fatal(err)
				}
			case "mismatched-receipt":
				receipt.Goal = "different"
				if err := writePiCreationRecord(s.creationPath("example"), receipt, maxCreationReceiptBytes); err != nil {
					t.Fatal(err)
				}
			case "unvalidated-receipt":
				receipt.Validated = false
				if err := writePiCreationRecord(s.creationPath("example"), receipt, maxCreationReceiptBytes); err != nil {
					t.Fatal(err)
				}
			case "wrong-native-generation":
				j.NativeGeneration = strings.Repeat("e", 64)
			case "wrong-native-result":
				j.Native.NativeID = "foreign-native"
			case "missing-mapping":
				f.state.Conversation.Member = "other0"
			case "mismatched-mapping":
				f.state.Settings.AccountRef = "foreign-account"
			case "unknown-input":
				f.submission.Phase = "Unknown"
			case "missing-witness":
				f.submission.Witness = nil
			case "unexpected-completion":
				if err := writePiCreationRecord(s.creationEvidencePath(receipt), map[string]any{"schema": 4}, 64<<10); err != nil {
					t.Fatal(err)
				}
			}
			if err := writePiCreationRecord(path, j, j.RecordLimit()); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "delete")
			ensures, creates, sends := f.ensures, f.creates, f.sends
			action := "capture-force-delete"
			if kind == "ordinary-capture" {
				action = "capture"
			}
			capture, err := s.piLifecycleOperation(context.Background(), action, "example", session.PiLifecycleCapture{})
			positive := kind == "exact" || kind == "native-result-absent"
			if (err == nil) != positive {
				t.Fatal(kind, err)
			}
			if positive {
				if len(capture.Members) != 2 || capture.Members[0].Conversation == nil || capture.Members[1].Conversation != nil || len(capture.Callbacks) != 1 {
					t.Fatal(capture)
				}
				for _, selector := range capture.Selectors {
					if selector.Name == "completion" {
						t.Fatal("synthesized completion")
					}
				}
				encoded, _ := json.Marshal(capture)
				if _, err := session.DecodePiLifecycleCapture(encoded, s.config.Workspace, "example"); err != nil {
					t.Fatal(err)
				}
				t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "archive")
				if _, err := s.piLifecycleOperation(context.Background(), "archive-retire", "example", capture); err == nil || f.retires != 0 {
					t.Fatal("creating delete capture authorized archive", err)
				}
				t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "delete")
				before, _ := json.Marshal(f.submission)
				result, err := s.piLifecycleOperation(context.Background(), "terminal-delete", "example", capture)
				after, _ := json.Marshal(f.submission)
				if err != nil || result.Terminal == nil || !reflect.DeepEqual(before, after) || f.retires != 1 {
					t.Fatal(result, err)
				}
				if _, err := s.piLifecycleOperation(context.Background(), "check", "example", capture); err != nil {
					t.Fatal(err)
				}
				if err := writePiCreationRecord(s.creationEvidencePath(receipt), map[string]any{"schema": 4}, 64<<10); err != nil {
					t.Fatal(err)
				}
				if _, err := s.piLifecycleOperation(context.Background(), "check", "example", capture); err == nil {
					t.Fatal("completion arrival ignored")
				}
			}
			if f.ensures != ensures || f.creates != creates || f.sends != sends || !positive && f.retires != 0 {
				t.Fatal("creation/input/native effect during capture")
			}
		})
	}
}

func TestPiLifecycleBorrowsExactParentWithoutUnlockingIt(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "example.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "archive")
	t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", strconv.Itoa(int(file.Fd())))
	t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", path)
	config := Config{AuthorityDir: directory}
	borrowed, err := piLifecycleParent(config, "example")
	if err != nil {
		t.Fatal(err)
	}
	borrowed.Close()
	probe, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("child released parent's exclusion", err)
	}
	for _, operation := range []string{"", "stop", "start"} {
		t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", operation)
		if _, err := piLifecycleParent(config, "example"); err == nil {
			t.Fatal("unadmitted lifecycle accepted", operation)
		}
	}
	t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "delete")
	t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", filepath.Join(directory, "other.lock"))
	if _, err := piLifecycleParent(config, "example"); err == nil {
		t.Fatal("wrong parent path accepted")
	}
}

var _ piLifecycleRuntime = (*piLifecycleFixture)(nil)
