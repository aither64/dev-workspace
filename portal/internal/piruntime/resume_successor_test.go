package piruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The existing fake systemd boundary delegates admission and Entry selection to
// the real Ruby host. Only native exec is simulated by the existing supervisor.
type rubySuccessorHost struct {
	*upgradeFixtureHost
	script           string
	failBeforeIntent bool
}

func (h *rubySuccessorHost) Stop(ctx context.Context, ref RuntimeRef, observed HostObservation) (HostObservation, error) {
	terminal, err := h.upgradeFixtureHost.Stop(ctx, ref, observed)
	if err != nil {
		return terminal, err
	}
	// Seed the already-completed predecessor receipt, not a successor authority.
	data, _ := json.Marshal(terminal)
	var entry map[string]any
	_ = json.Unmarshal(data, &entry)
	entry["schema"] = 1
	entry["stop_requested"] = true
	path := filepath.Join(h.base.m.instance(ref), "receipts", "host.json")
	err = writePrivate(path, map[string]any{"schema": 1, "instance_id": ref.InstanceID, "current_request_id": ref.RequestID, "requests": map[string]any{ref.RequestID: entry}})
	return terminal, err
}
func (h *rubySuccessorHost) Inspect(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	return h.ruby(ctx, "inspect", ref)
}
func (h *rubySuccessorHost) Start(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	if h.failBeforeIntent {
		return HostObservation{}, ErrUnavailable
	}
	observed, err := h.ruby(ctx, "start", ref)
	if err != nil || observed.Outcome != "running" {
		return observed, err
	}
	// Ruby has already persisted intent and selected the exact executable via Entry.
	if h.base.observation.RequestID != ref.RequestID {
		native, err := h.upgradeFixtureHost.Start(ctx, ref)
		if err != nil {
			return observed, err
		}
		if !sameExecution(native, observed) {
			return observed, ErrScope
		}
	}
	return observed, nil
}
func (h *rubySuccessorHost) ruby(ctx context.Context, verb string, ref RuntimeRef) (HostObservation, error) {
	m := h.base.m
	records, err := m.records(ref.Session)
	if err != nil {
		return HostObservation{}, err
	}
	record := records.Requests[ref.RequestID]
	var previous HostObservation
	for _, value := range h.terminal {
		previous = value
	}
	if previous.RequestID == "" {
		return h.base.Inspect(ctx, ref)
	}
	// Fake systemd reports the same execution fields as the existing Unix fixture.
	launch := launchFor(record.Start, ref)
	next := HostObservation{LaunchRequest: launch, Digest: ref.Digest, Outcome: "running", Phase: "running", BootID: "fake-boot", InvocationID: "fake-invocation", ControlGroup: "/fake-fixture", MainPID: os.Getpid(), PIDStart: "fake-start", MountNamespace: "fake-mnt", UserNamespace: "fake-user", LoadState: "loaded", ActiveState: "active", SubState: "running"}
	oldData, _ := json.Marshal(previous)
	newData, _ := json.Marshal(next)
	args := []string{h.script, m.config.HostStateRoot, ref.InstanceID, ref.RequestID, ref.Digest, verb, string(oldData), string(newData), record.Start.AppStorePath}
	command := exec.CommandContext(ctx, "ruby", args...)
	output, commandErr := command.Output()
	var wait *syscall.WaitStatus
	var exit *exec.ExitError
	if errors.As(commandErr, &exit) {
		if value, ok := exit.Sys().(syscall.WaitStatus); ok {
			wait = &value
		}
	}
	return nativeHostOutput(verb, ref, ctx.Err(), output, commandErr, wait)
}

func installRubySuccessorHost(t *testing.T, f *P6NativeFixture) func() {
	t.Helper()
	host := f.Manager.config.Host.(*upgradeFixtureHost)
	script := filepath.Join(t.TempDir(), "ruby-host.rb")
	source, err := filepath.Abs("../../../libexec/workspace-pi-host")
	if err != nil {
		t.Fatal(err)
	}
	code := `require 'json'
load ARGV.shift
root, instance, request, digest, verb, old_json, next_json, selected = ARGV
old, following = JSON.parse(old_json), JSON.parse(next_json)
old["job"] = following["job"] = ""
layout = WorkspacePiHost::Layout.new(root: root, instance: instance, uid: Process.uid)
manager = Object.new
manager.define_singleton_method(:observe) do |_unit|
 receipt = File.join(layout.directory,'receipts/host.json')
 entries = File.exist?(receipt) ? JSON.parse(File.read(receipt)) : {}
 current = entries['current_request_id']
 value = current == request && entries.fetch('requests',{}).fetch(request,{}).fetch('phase','') == 'running' ? following : old
 keys = %w[boot_id load_state active_state sub_state job invocation_id control_group main_pid main_pid_start mnt_namespace user_namespace]
 value.select { |key,_| keys.include?(key) }
end
manager.define_singleton_method(:empty?) { |_group| true }
manager.define_singleton_method(:start) do |_unit|
 WorkspacePiHost::Entry.run({'owner_uid'=>Process.uid,'state_root'=>root},instance,layout:layout,execer:lambda { |argv|
  raise 'wrong entry package' unless argv.first == File.join(selected,'bin/workspace-portal')
  File.write(File.join(layout.directory,'fixture-entry.json'),JSON.generate(argv))
 })
 manager.define_singleton_method(:observe) { |_u| following.select { |key,_| %w[boot_id load_state active_state sub_state job invocation_id control_group main_pid main_pid_start mnt_namespace user_namespace].include?(key) } }
end
controller=WorkspacePiHost::Controller.new(layout:layout,manager:manager,receipt_uid:Process.uid)
result=controller.call(verb:verb,instance:instance,request:request,digest:digest)
puts JSON.generate(result)
exit(result['outcome']=='rejected' ? 1 : 0)
`
	// The source path is an ordinary first script argument, never credential data.
	if err := os.WriteFile(script, []byte("ARGV.unshift("+strconvQuote(source)+")\n"+code), 0600); err != nil {
		t.Fatal(err)
	}
	adapter := &rubySuccessorHost{upgradeFixtureHost: host, script: script, failBeforeIntent: true}
	f.Manager.config.Host = adapter
	return func() { adapter.failBeforeIntent = false }
}
func strconvQuote(value string) string { data, _ := json.Marshal(value); return string(data) }

type successorRecoveryHost struct {
	*upgradeFixtureHost
	request     string
	inspectErr  error
	observation HostObservation
	attempts    int
}

func (h *successorRecoveryHost) Start(_ context.Context, _ RuntimeRef) (HostObservation, error) {
	h.attempts++
	return HostObservation{}, ErrUnavailable
}
func (h *successorRecoveryHost) Inspect(ctx context.Context, ref RuntimeRef) (HostObservation, error) {
	if ref.RequestID == h.request {
		return h.observation, h.inspectErr
	}
	return h.upgradeFixtureHost.Inspect(ctx, ref)
}

func TestResumeSuccessorStartRefusalsPreserveFrozenIntent(t *testing.T) {
	m, base, start, ctx := supervisorFixture(t)
	host := newUpgradeFixtureHost(base)
	m.config.Host = host
	m.config.validatePackage = func(string) error { return nil }
	m.config.verifyDrain = func(HostObservation) error { return nil }
	first, err := m.Ensure(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Create(ctx, first.Runtime, CreateRequest{Session: start.Session, Member: "lead", Incarnation: strings.Repeat("b", 64), CreationID: strings.Repeat("c", 64), Settings: settingsFixture()})
	if err != nil {
		t.Fatal(err)
	}
	handle := agentbackend.ExecutionHandle{Conversation: state.Conversation, Generation: state.Generation}
	if sent, err := m.Deliver(ctx, handle, backend.SendRequest{Message: "one retained original", ClientUserMessageID: strings.Repeat("d", 32)}); err != nil || sent.Outcome != backend.Accepted {
		t.Fatal(sent, err)
	}
	waitIdle(t, m, ctx, handle)
	next := nextStart(start, "22222222-2222-4222-8222-222222222222")
	next.BindingRevision = start.BindingRevision
	request := UpgradeRequest{Start: next, ExpectedGeneration: first.Runtime.Generation, Members: []agentbackend.ConversationRef{state.Conversation}}
	missed := &successorRecoveryHost{upgradeFixtureHost: host, request: next.RequestID, inspectErr: ErrHostIntentAbsent}
	m.config.Host = missed
	hooks := UpgradeHooks{EnsureAttachment: func(context.Context, RuntimeRef, HostCallbackConnector) error { return nil }, Publish: func(context.Context, UpgradeProof) error { t.Fatal("failed start published"); return ErrScope }}
	if _, err := m.Upgrade(ctx, request, hooks); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missed-intent setup", err)
	}
	records, err := m.records(start.Session)
	if err != nil {
		t.Fatal(err)
	}
	current := records.Requests[next.RequestID]
	if current.Phase != "starting" || missed.attempts != 1 || host.stops != 1 {
		t.Fatal("not a missed successor", current.Phase, missed.attempts, host.stops)
	}
	directory, _ := stateDirectory(m.config.UserStateRoot, start.Session)
	instance := m.instance(current.Runtime)
	memberPath := filepath.Join(directory, "members", state.Conversation.Member, state.Conversation.Incarnation, "conversation.json")
	paths := []string{filepath.Join(directory, "runtime.json"), memberPath, filepath.Join(instance, "launch.json"), filepath.Join(instance, "requests", next.RequestID, "launch.json"), filepath.Join(instance, "requests", next.RequestID, "runtime-config.json"), filepath.Join(instance, "requests", start.RequestID, "launch.json")}
	original := map[string][]byte{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		original[p] = data
	}
	for _, fault := range []string{"unknown", "unavailable", "malformed", "cancel", "request", "predecessor", "profile", "target", "launch", "config", "old-launch", "root", "member-loaded", "effect-held", "drain-live", "drain-unproved", "lock-busy", "present-intent"} {
		t.Run(fault, func(t *testing.T) {
			candidate := request
			callContext := ctx
			restore := func() {}
			switch fault {
			case "unknown":
				missed.inspectErr = nil
				missed.observation = HostObservation{}
			case "unavailable":
				missed.inspectErr = ErrUnavailable
			case "malformed":
				missed.inspectErr = errors.New("malformed helper response")
			case "cancel":
				var cancel context.CancelFunc
				callContext, cancel = context.WithCancel(ctx)
				cancel()
			case "request":
				candidate.Start.RequestID = "33333333-3333-4333-8333-333333333333"
			case "predecessor":
				candidate.ExpectedGeneration = strings.Repeat("f", 64)
			case "profile":
				candidate.Start.ProfileGeneration = "changed"
			case "target":
				candidate.Start.AppStorePath += "-changed"
			case "launch":
				os.WriteFile(filepath.Join(instance, "launch.json"), []byte("{}"), 0600)
			case "config":
				os.WriteFile(filepath.Join(instance, "requests", next.RequestID, "runtime-config.json"), []byte("{}"), 0600)
			case "old-launch":
				os.WriteFile(filepath.Join(instance, "requests", start.RequestID, "launch.json"), []byte("{}"), 0600)
			case "root":
				p := filepath.Join(instance, "requests", next.RequestID, "package-root")
				os.Remove(p)
				os.Symlink(next.AppStorePath+"-changed", p)
				restore = func() { os.Remove(p); os.Symlink(next.AppStorePath, p) }
			case "member-loaded", "effect-held":
				var member conversationRecord
				if err := readPrivate(memberPath, &member); err != nil {
					t.Fatal(err)
				}
				if fault == "member-loaded" {
					member.Loaded = true
				} else {
					member.HeldReason = "unresolved effect"
				}
				if err := writePrivate(memberPath, member); err != nil {
					t.Fatal(err)
				}
			case "drain-live":
				m.config.verifyDrain = func(HostObservation) error { return ErrHeld }
			case "drain-unproved":
				old := host.terminal[start.RequestID]
				bad := old
				bad.DrainProof = nil
				host.terminal[start.RequestID] = bad
				restore = func() { host.terminal[start.RequestID] = old }
			case "lock-busy":
				release, err := runtimeLock(directory)
				if err != nil {
					t.Fatal(err)
				}
				restore = release
			case "present-intent":
				missed.inspectErr = nil
				missed.observation = HostObservation{LaunchRequest: launchFor(next, current.Runtime), Digest: current.Runtime.Digest, Outcome: "unknown", Phase: "unknown"}
			}
			before := map[string][]byte{}
			for _, p := range paths {
				before[p], err = os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
			}
			status, err := m.ResumeSuccessorStart(callContext, candidate)
			if fault == "present-intent" {
				if err != nil || status.Phase != "unknown" {
					t.Fatal(status, err)
				}
			} else if err == nil {
				t.Fatal("invalid recovery admitted")
			}
			if missed.attempts != 1 || host.stops != 1 {
				t.Fatal("refusal repeated native effect", missed.attempts, host.stops)
			}
			for p, data := range before {
				after, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatal("refusal mutated frozen input", p, err)
				}
			}
			restore()
			for p, data := range original {
				if err := os.WriteFile(p, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			m.config.verifyDrain = func(HostObservation) error { return nil }
			missed.inspectErr = ErrHostIntentAbsent
			missed.observation = HostObservation{}
		})
	}
}
