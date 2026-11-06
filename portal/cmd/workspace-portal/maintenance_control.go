package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/coder/websocket"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

type maintenanceControl struct {
	store                                  maintenance.Store
	workspace                              string
	invocation                             string
	pauseCallTimeout, pauseRecoveryTimeout time.Duration
}

func maintenanceControlCommand(args []string, input io.Reader, output io.Writer) error {
	f := flag.NewFlagSet("maintenance-control", flag.ContinueOnError)
	root := f.String("user-state-root", "", "private state root")
	profile := f.String("host-profile", "", "selected profile")
	workspace := f.String("workspace", "", "registered workspace")
	worker := f.Bool("worker", false, "host worker actions")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 || !canonicalAbsolutePath(*root) || !canonicalAbsolutePath(*profile) || !canonicalAbsolutePath(*workspace) {
		return errors.New("maintenance control requires exact host authorities")
	}
	data, e := io.ReadAll(io.LimitReader(input, maintenance.MaxBytes+1))
	if e != nil {
		return e
	}
	if len(data) > maintenance.MaxBytes {
		return errors.New("maintenance request exceeds bound")
	}
	var envelope struct {
		Action      string                `json:"action"`
		OperationID string                `json:"operationId"`
		Operation   *maintenance.Identity `json:"operation"`
	}
	if e = maintenance.DecodeEnvelope(data, &envelope); e != nil {
		return errors.New("invalid maintenance envelope")
	}
	routing := maintenance.Request{Action: envelope.Action, OperationID: envelope.OperationID}
	if envelope.Operation != nil {
		v := envelope.Operation
		routing.Operation = &maintenance.Operation{Schema: v.Schema, OperationID: v.OperationID, Profile: v.Profile, SourcePackage: v.SourcePackage}
	}
	var r maintenance.Request
	// Only resolve the immutable protocol owner here. Older schema1 journals
	// retain their own phase/Finished semantics across profile changes.
	owner, err := maintenanceProtocolOwner(maintenance.Store{Root: *root, Profile: *profile}, routing)
	if err != nil {
		return err
	}
	if owner != "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		current, err := filepath.EvalSymlinks(executable)
		if err != nil {
			return err
		}
		if owner != filepath.Dir(filepath.Dir(current)) {
			command := exec.Command(filepath.Join(owner, "bin/workspace-portal"), append([]string{"maintenance-control"}, args...)...)
			command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(data), output, os.Stderr
			return command.Run()
		}
	}
	if e = teamMCPDecode(data, &r); e != nil {
		return errors.New("invalid maintenance request")
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	m := maintenanceControl{store: maintenance.Store{Root: *root, Profile: *profile}, workspace: *workspace}
	var result maintenance.Response
	if *worker {
		result, e = m.worker(ctx, r)
	} else {
		result, e = (maintenance.Controller{Store: m.store}).Handle(ctx, r, false)
	}
	if e != nil {
		result = maintenance.Response{Schema: 1, Status: "refused", Reason: maintenanceControlFailureReason(e)}
	}
	return json.NewEncoder(output).Encode(result)
}

func maintenanceProtocolOwner(store maintenance.Store, r maintenance.Request) (string, error) {
	if r.OperationID == "" && r.Action != "active" && r.Action != "status" {
		return "", nil
	}
	var o maintenance.Identity
	var err error
	if r.Action == "prepare" && r.Operation != nil {
		o = maintenance.Identity{Schema: r.Operation.Schema, OperationID: r.Operation.OperationID, Profile: r.Operation.Profile, SourcePackage: r.Operation.SourcePackage}
	} else {
		o, err = store.Identity(r.OperationID)
		if errors.Is(err, os.ErrNotExist) && (r.OperationID == "" || strings.HasPrefix(r.Action, "startup-")) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
	owner, err := filepath.EvalSymlinks(o.SourcePackage)
	if err != nil || owner != o.SourcePackage || (r.OperationID != "" && o.OperationID != r.OperationID) || o.Profile != store.Profile {
		return "", errors.New("maintenance protocol owner unavailable or changed")
	}
	for _, name := range []string{"workspace-host", "workspace-portal"} {
		info, err := os.Stat(filepath.Join(owner, "bin", name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return "", errors.New("maintenance protocol owner binary unavailable")
		}
	}
	return owner, nil
}

// This is a target host behavior capability, independent of schema1 reader
// compatibility and of the source native's separate admission contract.
func requireMaintenanceHeldTarget(packageRoot string) error {
	file, err := os.Open(filepath.Join(packageRoot, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return errors.New("maintenance target descriptor exceeds bound")
	}
	var descriptor map[string]json.RawMessage
	if err := teamMCPDecode(data, &descriptor); err != nil {
		return err
	}
	// Reject duplicate descriptor keys as the Ruby admission reader does.
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("invalid maintenance target descriptor")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return errors.New("duplicate maintenance target descriptor key")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	for _, key := range []string{"maintenancePolicy", "maintenanceStateSchema"} {
		var value int
		if json.Unmarshal(descriptor[key], &value) != nil || value != 2 {
			return errors.New("maintenance target lacks bounded pause contract")
		}
	}
	var policy int
	if json.Unmarshal(descriptor["maintenanceHeldCompletionPolicy"], &policy) != nil || policy != 1 {
		return errors.New("maintenance target cannot keep completion held")
	}
	return nil
}

func (m maintenanceControl) service(o maintenance.Operation, p maintenance.Participant) (maintenance.Service, error) {
	for _, s := range o.Services {
		if s.Workspace == p.Workspace {
			return s, nil
		}
	}
	return maintenance.Service{}, errors.New("maintenance service absent")
}
func (m maintenanceControl) client(o maintenance.Operation, p maintenance.Participant) *codex.Client {
	s, _ := m.service(o, p)
	return m.clientForSocket(s.Socket)
}

func processIdentity(pid int) (string, error) {
	data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return "", e
	}
	tail := strings.LastIndex(string(data), ") ")
	if tail < 0 {
		return "", errors.New("invalid native process identity")
	}
	fields := strings.Fields(string(data)[tail+2:])
	if len(fields) < 20 {
		return "", errors.New("invalid native process identity")
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("%s:%d:%s", strings.TrimSpace(string(boot)), pid, fields[19]), nil
}
func trackingIncarnation(workspace, slug string) (string, error) {
	info, e := os.Stat(filepath.Join(workspace, "work", slug))
	if e != nil {
		return "", e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("tracking incarnation unavailable")
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

type inventoryUnavailable struct{ error }

func (m maintenanceControl) sourceCurrent(s maintenance.Service) error {
	process, e := processIdentity(s.PID)
	if e != nil || process != s.ProcessIdentity {
		return &maintenance.ProofError{Reason: "source-process-changed"}
	}
	exe, e := os.Readlink(fmt.Sprintf("/proc/%d/exe", s.PID))
	if e != nil || exe != s.Native {
		return &maintenance.ProofError{Reason: "source-native-changed"}
	}
	epoch, e := session.SocketIdentity(s.Socket)
	if e != nil || epoch != s.SocketEpoch {
		return &maintenance.ProofError{Reason: "source-socket-changed"}
	}
	return nil
}
func (m maintenanceControl) inventoryFailure(s maintenance.Service, err error) error {
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.DeadlineExceeded) || websocket.CloseStatus(err) != -1 {
		if proofErr := m.sourceCurrent(s); proofErr != nil {
			return proofErr
		}
		return &inventoryUnavailable{err}
	}
	return err
}

func (m maintenanceControl) identity(ctx context.Context, o maintenance.Operation, p maintenance.Participant) error {
	s, e := m.service(o, p)
	if e != nil {
		return e
	}
	epoch, e := session.SocketIdentity(s.Socket)
	if e != nil || epoch != p.SocketEpoch || epoch != s.SocketEpoch {
		return &maintenance.ProofError{Reason: "source-socket-changed"}
	}
	process, e := processIdentity(s.PID)
	if e != nil || process != s.ProcessIdentity {
		return &maintenance.ProofError{Reason: "source-process-changed"}
	}
	exe, e := os.Readlink(fmt.Sprintf("/proc/%d/exe", s.PID))
	if e != nil || exe != s.Native {
		return &maintenance.ProofError{Reason: "source-native-changed"}
	}
	return m.retainedIdentity(p, s)
}
func (m maintenanceControl) retainedIdentity(p maintenance.Participant, s maintenance.Service) error {
	summary, e := session.Find(p.Workspace, p.Slug)
	if e != nil || summary.Archived || summary.Codex.ThreadID != p.Root || summary.Codex.SocketPath != s.Socket {
		return &maintenance.ProofError{Reason: "session-identity-changed"}
	}
	inc, e := trackingIncarnation(p.Workspace, p.Slug)
	if e != nil || inc != p.Incarnation {
		return &maintenance.ProofError{Reason: "session-incarnation-changed"}
	}
	if owner, e := session.PendingLifecycle(p.Workspace, p.Slug); e != nil || owner != "" {
		return &maintenance.ProofError{Reason: "session-lifecycle-unfinished"}
	}
	if p.Thread == p.Root {
		return nil
	}
	store, e := teamruntime.NewStore(m.store.Root, p.Workspace)
	if e != nil {
		return e
	}
	roster, e := store.Load(p.Slug, p.Root)
	if e != nil {
		return e
	}
	for _, member := range roster.Members {
		if member.Thread == p.Thread && member.Address == p.Member && member.State == "ready" && member.RetireIntent == "" {
			return nil
		}
	}
	return &maintenance.ProofError{Reason: "retained-member-changed"}
}

func (m maintenanceControl) worker(ctx context.Context, r maintenance.Request) (maintenance.Response, error) {
	if r.Schema != 1 {
		return maintenance.Response{}, errors.New("invalid maintenance request")
	}
	if strings.HasPrefix(r.Action, "startup-") {
		var e error
		switch r.Action {
		case "startup-status":
		case "startup-reconcile":
			return m.reconcileStartup(ctx, r)
		case "startup-stage":
			if r.Startup == nil {
				return maintenance.Response{}, errors.New("startup target required")
			}
			e = m.store.StageStartup(ctx, r.OperationID, r.Startup.CandidatePackage, r.Startup.SourceProfileToken, r.Startup.TargetEnabled)
		case "startup-authorize":
			if r.Startup == nil {
				return maintenance.Response{}, errors.New("startup authority required")
			}
			e = m.store.AuthorizeStartupGrants(ctx, r.OperationID, r.Startup.CandidatePackage, r.Startup.AuthorizedProfileToken, r.Startup.LaunchGrants)
		case "startup-finish":
			if r.Startup == nil {
				return maintenance.Response{}, errors.New("startup completion required")
			}
			e = m.store.FinishStartup(ctx, r.OperationID, r.Startup.CandidatePackage, r.Startup.AuthorizedProfileToken)
		case "startup-launch":
			if r.Startup == nil {
				return maintenance.Response{}, errors.New("startup launch identity required")
			}
			if r.Grant == nil {
				return maintenance.Response{}, errors.New("startup grant required")
			}
			enabled, err := m.consumeStartup(ctx, r)
			if err != nil {
				return maintenance.Response{}, err
			}
			status := "disabled"
			if enabled {
				status = "enabled"
			}
			return maintenance.Response{Schema: 1, Status: status}, nil
		default:
			return maintenance.Response{}, errors.New("unknown startup action")
		}
		if e != nil {
			return maintenance.Response{}, e
		}
		state, e := m.store.Startup()
		status := "disabled"
		if state.Enabled {
			status = "enabled"
		}
		if state.Pending {
			status = "pending"
		}
		return maintenance.Response{Schema: 1, Status: status, Startup: &state}, e
	}
	if r.Action == "active" {
		o, e := m.store.Active()
		if errors.Is(e, os.ErrNotExist) {
			return maintenance.Response{Schema: 1, Status: "none"}, nil
		}
		return maintenance.Response{Schema: 1, Status: o.Phase, Operation: &o}, e
	}
	if r.Action == "process-identity" {
		return maintenance.Response{}, errors.New("process identity requires captured service")
	}
	if r.Action == "prepare" && r.Operation != nil {
		if r.OperationID != r.Operation.OperationID {
			return maintenance.Response{}, errors.New("operation identity changed")
		}
		o := *r.Operation
		var prepared struct{ CandidatePackage string }
		if json.Unmarshal(o.Preparation, &prepared) != nil || prepared.CandidatePackage != o.CandidatePackage || !canonicalAbsolutePath(o.CandidatePackage) {
			return maintenance.Response{}, errors.New("prepared maintenance target changed")
		}
		if err := requireMaintenanceHeldTarget(o.CandidatePackage); err != nil {
			return maintenance.Response{}, err
		}
		for i := range o.Services {
			s := &o.Services[i]
			var e error
			s.ProcessIdentity, e = processIdentity(s.PID)
			if e != nil {
				return maintenance.Response{}, e
			}
			s.Native, e = os.Readlink(fmt.Sprintf("/proc/%d/exe", s.PID))
			if e != nil {
				return maintenance.Response{}, e
			}
			s.SocketEpoch, e = session.SocketIdentity(s.Socket)
			if e != nil {
				return maintenance.Response{}, e
			}

		}
		if e := m.inventory(ctx, &o); e != nil {
			return maintenance.Response{}, e
		}
		if e := m.store.Create(ctx, o); e != nil {
			return maintenance.Response{}, e
		}
		return (maintenance.Controller{Store: m.store}).Status(o.OperationID)
	}
	if r.Action == "worker-claim" {
		if err := m.store.RecoverPointer(ctx, r.OperationID); err != nil {
			return maintenance.Response{}, err
		}
	}
	o, e := m.store.Load(r.OperationID)
	if e != nil {
		return maintenance.Response{}, e
	}
	m.invocation = r.Invocation
	if r.Action != "worker-claim" && r.Action != "inspect" && o.WorkerInvocation != "" && o.WorkerInvocation != m.invocation {
		return maintenance.Response{}, errors.New("maintenance worker invocation changed")
	}
	switch r.Action {
	case "source-current":
		for _, s := range o.Services {
			process, err := processIdentity(s.PID)
			if o.CutoverIntent && errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || process != s.ProcessIdentity {
				return maintenance.Response{}, errors.New("source process changed")
			}
			epoch, err := session.SocketIdentity(s.Socket)
			if err != nil || epoch != s.SocketEpoch {
				return maintenance.Response{}, errors.New("source socket changed")
			}
		}
		return maintenance.Response{Schema: 1, Status: "source-current"}, nil
	case "recovery-required":
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if !current.Terminal() {
				current.Phase = "recovery-required"
				found := false
				for _, reason := range current.Blockers {
					if reason == "worker-recovery-required" {
						found = true
					}
				}
				if !found {
					current.Blockers = append(current.Blockers, "worker-recovery-required")
				}
			}
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "recovery-required"}, e
	case "worker-claim":
		if r.Invocation == "" || r.Operation == nil {
			return maintenance.Response{}, errors.New("worker invocation identity required")
		}
		_, e = m.store.Update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if current.WorkerInvocation != "" && current.WorkerInvocation != r.Invocation && current.CutoverIntent && !current.ActivationIntent {
				if r.Operation.SourcePackage != current.SourcePackage || r.Operation.SourceProfileToken != current.SourceProfileToken {
					return errors.New("unadmitted target effects or source generation changed")
				}
				current.SourceRecovered = true
			}
			current.WorkerInvocation = r.Invocation
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "worker-owned"}, e
	case "activation-intent":
		if r.Operation == nil {
			return maintenance.Response{}, errors.New("activation target required")
		}
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if !current.CutoverIntent {
				return errors.New("source cutover missing")
			}
			if !current.SourcesStopped {
				return errors.New("source shutdown incomplete")
			}
			var prepared struct{ CandidatePackage, CandidateCodex, PlansDigest, SourcePackage, SourceCodex, SourcePlansDigest string }
			if json.Unmarshal(current.Preparation, &prepared) != nil {
				return errors.New("prepared target unavailable")
			}
			target, native, plans := prepared.CandidatePackage, prepared.CandidateCodex, prepared.PlansDigest
			if current.SourceRecovered {
				target, native, plans = prepared.SourcePackage, prepared.SourceCodex, prepared.SourcePlansDigest
			}
			requested := r.Operation
			if target == "" || native == "" || plans == "" || requested.ActivationPackage != target || requested.ActivationNative != native || requested.ActivationPlansDigest != plans {
				return errors.New("activation target changed")
			}
			if current.ActivationIntent && (current.ActivationPackage != target || current.ActivationNative != native || current.ActivationPlansDigest != plans) {
				return errors.New("activation already admitted")
			}
			if err := requireMaintenanceHeldTarget(target); err != nil {
				return err
			}
			current.ActivationIntent = true
			current.ActivationPackage = target
			current.ActivationNative = native
			current.ActivationPlansDigest = plans
			current.Phase = "activating"
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "activation-admitted"}, e
	case "destination-ready":
		if r.Operation == nil || len(r.Operation.Services) != len(o.Services) || !o.ActivationIntent {
			return maintenance.Response{}, errors.New("destination identity required")
		}
		destination := append([]maintenance.Service(nil), o.Services...)
		for i, s := range destination {
			proposed := r.Operation.Services[i]
			if s.Unit != proposed.Unit || s.Workspace != proposed.Workspace {
				return maintenance.Response{}, errors.New("destination service changed")
			}
			epoch, err := session.SocketIdentity(s.Socket)
			if err != nil || epoch == s.SocketEpoch {
				return maintenance.Response{}, errors.New("destination epoch unproved")
			}
			process, err := processIdentity(proposed.PID)
			if err != nil {
				return maintenance.Response{}, err
			}
			exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", proposed.PID))
			if err != nil {
				return maintenance.Response{}, err
			}
			destination[i].DestinationEpoch = epoch
			destination[i].DestinationPID = proposed.PID
			destination[i].DestinationProcessIdentity = process
			destination[i].DestinationNative = exe
		}
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			for i, s := range destination {
				old := current.Services[i]
				if old.DestinationEpoch != "" && (old.DestinationEpoch != s.DestinationEpoch || old.DestinationProcessIdentity != s.DestinationProcessIdentity) {
					return errors.New("destination incarnation changed")
				}
				current.Services[i] = s
			}
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "destination-ready"}, e
	case "sources-stopped":
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if !current.CutoverIntent {
				return errors.New("source stop not owned")
			}
			for _, s := range current.Services {
				process, err := processIdentity(s.PID)
				if err == nil && process == s.ProcessIdentity {
					return errors.New("owned source process remains")
				}
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			current.SourcesStopped = true
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "sources-stopped"}, e
	case "writers-drained":
		if r.Operation == nil {
			return maintenance.Response{}, errors.New("writer checkpoint required")
		}
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if current.CutoverIntent {
				return errors.New("writer checkpoint after cutover")
			}
			current.DrainedPortals = r.Operation.DrainedPortals
			current.Phase = "draining-writers"
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "draining-writers"}, e
	case "restoration-begin":
		if r.Restoration == nil {
			return maintenance.Response{}, errors.New("restoration intent required")
		}
		return (maintenance.Controller{Store: m.store, WorkerInvocation: m.invocation}).BeginRestoration(ctx, o.OperationID, *r.Restoration)
	case "restoration-complete":
		if r.Restoration == nil {
			return maintenance.Response{}, errors.New("restoration receipt required")
		}
		return (maintenance.Controller{Store: m.store, WorkerInvocation: m.invocation}).FinishRestoration(ctx, o.OperationID, *r.Restoration)
	case "terminal-sync":
		return m.terminalSync(ctx, r)
	case "terminal-restoration":
		for _, intent := range o.TerminalIntents {
			summary, err := session.Find(intent.Workspace, intent.Slug)
			inc, incErr := trackingIncarnation(intent.Workspace, intent.Slug)
			if err != nil || incErr != nil || summary.Archived || summary.Codex.ThreadID != intent.Root || inc != intent.Incarnation {
				return maintenance.Response{}, errors.New("terminal restoration identity changed")
			}
		}
		return maintenance.Response{Schema: 1, Status: "terminal-restoration", Operation: &o}, nil
	case "terminal-intent":
		if o.CutoverIntent || o.CancelRequested || o.Restoration != nil || !session.ValidSlug(r.Slug) {
			return maintenance.Response{}, errors.New("terminal maintenance proof unavailable")
		}
		if o.Phase != "draining-writers" {
			return maintenance.Response{}, errors.New("bounded pause not settled")
		}
		terminalSummary, identityErr := session.Find(m.workspace, r.Slug)
		incarnation, incErr := trackingIncarnation(m.workspace, r.Slug)
		if identityErr != nil || incErr != nil || terminalSummary.Archived || terminalSummary.Codex.ThreadID == "" {
			return maintenance.Response{}, errors.New("terminal identity unavailable")
		}
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if current.CancelRequested || current.Restoration != nil {
				return errors.New("terminal quiesce admission ended")
			}
			for _, t := range current.TerminalIntents {
				if t.Workspace == m.workspace && t.Slug == r.Slug {
					if t.Root != terminalSummary.Codex.ThreadID || t.Incarnation != incarnation {
						return errors.New("terminal incarnation changed")
					}
					return nil
				}
			}
			current.TerminalIntents = append(current.TerminalIntents, maintenance.TerminalIntent{Workspace: m.workspace, Slug: r.Slug, Root: terminalSummary.Codex.ThreadID, Incarnation: incarnation})
			return nil
		})
		return maintenance.Response{Schema: 1, Status: "terminal-authorized"}, e
	case "inspect":
		return maintenance.Response{Schema: 1, Status: o.Phase, Operation: &o}, nil
	case "pause":
		return m.pause(ctx, o.OperationID)
	case "stop-intent":
		cancelled := false
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if current.CancelRequested {
				cancelled = true
				return nil
			}
			if current.Phase != "draining-writers" && !current.CutoverIntent {
				return errors.New("bounded pause not settled")
			}
			current.CutoverIntent = true
			current.Phase = "stopping-native"
			return nil
		})
		if cancelled {
			return maintenance.Response{Schema: 1, Status: "cancelled"}, e
		}
		return maintenance.Response{Schema: 1, Status: "stopping-native"}, e
	case "continue":
		return m.continueParticipants(ctx, o.OperationID)
	case "cancelled":
		_, e = m.update(ctx, o.OperationID, 0, func(current *maintenance.Operation) error {
			if current.CutoverIntent {
				return errors.New("native drain already owned")
			}
			current.CancelRequested = true
			current.Phase = "resuming"
			return nil
		})
		if e != nil {
			return maintenance.Response{}, e
		}
		return m.cancelNotices(ctx, o.OperationID)
	case "cancel-notices":
		return m.cancelNotices(ctx, o.OperationID)
	default:
		return (maintenance.Controller{Store: m.store}).Handle(ctx, r, true)
	}
}

func maintenanceUUID(seed string) string {
	d := sha256.Sum256([]byte(seed))
	s := hex.EncodeToString(d[:16])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func (m maintenanceControl) inventory(ctx context.Context, o *maintenance.Operation) error {
	o.Participants = []maintenance.Participant{}
	o.Blockers = []string{}
	o.Children = []maintenance.Child{}
	for _, s := range o.Services {
		if e := m.sourceCurrent(s); e != nil {
			return e
		}
		p := maintenance.Participant{Workspace: s.Workspace}
		client := m.client(*o, p)
		summaries, e := session.List(s.Workspace)
		if e != nil {
			client.Close()
			return e
		}
		retained := map[string]maintenance.Participant{}
		utilities := map[string]bool{}
		monitorStore, e := monitoroperation.NewStore(m.store.Root, s.Workspace)
		if e != nil {
			client.Close()
			return e
		}
		proofClient := newTransitionClient(s.Socket, s.Workspace, m.store.Root)
		defer proofClient.Close()
		store, e := teamruntime.NewStore(m.store.Root, s.Workspace)
		if e != nil {
			client.Close()
			return e
		}
		for _, summary := range summaries {
			if summary.Archived || summary.Codex.ThreadID == "" || summary.Codex.SocketPath != s.Socket {
				continue
			}
			inc, e := trackingIncarnation(s.Workspace, summary.Slug)
			if e != nil {
				client.Close()
				return e
			}
			base := maintenance.Participant{Workspace: s.Workspace, Slug: summary.Slug, Incarnation: inc, Root: summary.Codex.ThreadID, Thread: summary.Codex.ThreadID, Cwd: filepath.Join(s.Workspace, "work", summary.Slug), SocketEpoch: s.SocketEpoch}
			if prior := retained[base.Thread]; prior.Thread != "" {
				client.Close()
				return errors.New("ambiguous retained root ownership")
			}
			retained[base.Thread] = base
			view, proofErr := maintenanceMonitorInventory(ctx, monitorStore, summary, s.Socket, proofClient)
			if proofErr != nil {
				o.Blockers = append(o.Blockers, "monitor-operation-unresolved")
			}
			for _, utility := range view.Threads {
				if utilities[utility.ThreadID] || retained[utility.ThreadID].Thread != "" {
					client.Close()
					return errors.New("monitor maintenance thread ownership is ambiguous")
				}
				utilities[utility.ThreadID] = true
			}
			roster, e := store.Load(summary.Slug, summary.Codex.ThreadID)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				client.Close()
				return e
			}
			if roster == nil {
				continue
			}
			for _, member := range roster.Members {
				if member.State == "removed" || member.State == "archived" {
					continue
				}
				if member.State != "ready" || member.RetireIntent != "" {
					o.Blockers = append(o.Blockers, "retained-member-transition")
					continue
				}
				mp := base
				mp.Thread = member.Thread
				mp.Member = member.Address
				if mp.Thread == "" || retained[mp.Thread].Thread != "" || utilities[mp.Thread] {
					client.Close()
					return errors.New("ambiguous retained member ownership")
				}
				retained[mp.Thread] = mp
			}
		}
		children, e := maintenanceChildren(ctx, client, retained, utilities)
		if e != nil {
			client.Close()
			return e
		}
		for _, child := range children {
			o.Children = append(o.Children, maintenance.Child{Workspace: s.Workspace, Parent: child.Parent, Thread: child.Thread})
		}
		childIDs := map[string]bool{}
		for _, child := range children {
			childIDs[child.Thread] = true
		}
		loaded, e := client.LoadedThreadIDs(ctx)
		if e != nil {
			client.Close()
			return m.inventoryFailure(s, e)
		}
		for _, thread := range loaded {
			if utilities[thread] {
				if retained[thread].Thread != "" {
					client.Close()
					return errors.New("utility became a retained maintenance participant")
				}
				continue
			}
			actor, known := retained[thread]
			if !known {
				if childIDs[thread] {
					continue
				}
				o.Blockers = append(o.Blockers, "unowned-loaded-writer")
				continue
			}

			state, e := client.ReadTurnState(ctx, thread)
			if e != nil {
				client.Close()
				return m.inventoryFailure(s, e)
			}
			if state.LatestTurnStatus != "inProgress" {
				continue
			}
			actor.SourceTurn = state.LatestTurnID
			actor.NoticeID = maintenanceUUID(o.OperationID + ":" + thread + ":notice")
			actor.ContinuationID = maintenanceUUID(o.OperationID + ":" + thread + ":continue")
			actor.NoticeAction = "workspace-maintenance:" + o.OperationID
			actor.NoticeDisposition = "prepared"
			actor.ContinuationDisposition = "pending"
			actor.NoticeText = "A prepared workspace update will restart this workspace. Save useful context and a checkpoint, settle or interrupt your owned children, and finish your current turn without starting more work. Remaining work will be interrupted at the pause deadline. Leave queued prompts and goal state intact. After activation you will receive a resume command; inspect saved state and partial effects before retrying."
			o.Participants = append(o.Participants, actor)
		}
		client.Close()
	}
	if len(o.Blockers) > 0 {
		return errors.New("maintenance preparation contains unowned or unresolved native work")
	}
	return nil
}

func (m maintenanceControl) terminalSync(ctx context.Context, r maintenance.Request) (maintenance.Response, error) {
	status := "terminal-sync-admitted"
	// Keep Stop and the owned tail stable while publishing only this recorded
	// root's permission. No operation transition or native RPC belongs here.
	checked := errors.New("terminal recovery permission checked")
	_, err := m.update(ctx, r.OperationID, 0, func(o *maintenance.Operation) error {
		active, err := m.store.Active()
		if err != nil {
			return err
		}
		if active.OperationID != o.OperationID || o.WorkerInvocation == "" || o.WorkerInvocation != r.Invocation || o.Terminal() || o.Restoration == nil || o.Restoration.Completed {
			return errors.New("client restoration not owned")
		}
		selected, err := filepath.EvalSymlinks(m.store.Profile)
		if err != nil || selected != o.Restoration.Package {
			return errors.New("restoration selected package changed")
		}
		for _, intent := range o.TerminalIntents {
			if intent.Workspace != m.workspace || intent.Slug != r.Slug || intent.Root != r.ThreadID {
				continue
			}
			p := maintenance.Participant{Workspace: intent.Workspace, Slug: intent.Slug, Root: intent.Root, Thread: intent.Root, Incarnation: intent.Incarnation}
			service, err := m.service(*o, p)
			if err != nil {
				return err
			}
			if err := m.retainedIdentity(p, service); err != nil {
				return err
			}
			epoch := service.SocketEpoch
			if o.Restoration.Outcome == "cancelled" {
				if err := m.sourceCurrent(service); err != nil {
					return err
				}
			} else {
				epoch, err = m.destinationServiceIdentity(*o, service)
				if err != nil {
					return err
				}
			}
			for _, participant := range o.Participants {
				if participant.Workspace == intent.Workspace && participant.Thread == intent.Root && participant.StopRevision != 0 {
					status = "terminal-sync-suppressed"
					return checked
				}
			}
			recovery := session.RecoveryStore{Root: m.store.Root, Workspace: intent.Workspace}
			err = recovery.Update(ctx, intent.Slug, func(record *session.Recovery) error {
				if record.Schema != 1 || record.Workspace != intent.Workspace || record.Slug != intent.Slug || record.ThreadID != intent.Root || record.SocketPath != service.Socket || (record.SocketIdentity != service.SocketEpoch && record.SocketIdentity != epoch) {
					return errors.New("terminal recovery identity changed")
				}
				currentEpoch, err := session.SocketIdentity(service.Socket)
				if err != nil || currentEpoch != epoch {
					return errors.New("terminal recovery socket changed")
				}
				if !record.Automatic {
					status = "terminal-sync-suppressed"
					return checked
				}
				if record.SocketIdentity != epoch {
					record.ActiveThreads = []string{}
					record.Continuation = nil
				}
				record.SocketIdentity = epoch
				if !slices.Contains(record.ActiveThreads, intent.Root) {
					record.ActiveThreads = append(record.ActiveThreads, intent.Root)
				} else {
					return checked
				}
				return nil
			})
			if err != nil && !errors.Is(err, checked) {
				return err
			}
			return checked
		}
		return errors.New("client restoration identity changed")
	})
	// Abort the read-only operation update after the separately serialized
	// permission write, retaining the same incomplete restoration for retries.
	if errors.Is(err, checked) {
		return maintenance.Response{Schema: 1, Status: status}, nil
	}
	return maintenance.Response{}, err
}

func (m maintenanceControl) update(ctx context.Context, id string, revision uint64, change func(*maintenance.Operation) error) (maintenance.Operation, error) {
	return m.store.Update(ctx, id, revision, func(o *maintenance.Operation) error {
		if o.WorkerInvocation != "" && o.WorkerInvocation != m.invocation {
			return errors.New("maintenance worker was superseded")
		}
		return change(o)
	})
}

func maintenanceControlFailureReason(err error) string {
	var refusal *maintenance.ProofError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "maintenance-timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "maintenance-cancelled"
	}
	// Only inspected completion-proof literals cross this diagnostic boundary.
	switch err.Error() {
	case "invalid maintenance startup selection":
		return "invalid-maintenance-startup-selection"
	case "invalid startup grant state":
		return "invalid-startup-grant-state"
	case "invalid startup attempt sequence":
		return "invalid-startup-attempt-sequence"
	case "invalid consumed startup attempt":
		return "invalid-consumed-startup-attempt"
	case "startup invocation has no process evidence":
		return "startup-invocation-has-no-process-evidence"
	case "startup completion required":
		return "startup-completion-required"
	case "startup completion proof changed":
		return "startup-completion-proof-changed"
	case "startup grant has not reached native launch":
		return "startup-grant-has-not-reached-native-launch"
	case "startup reconciliation tuple changed":
		return "startup-reconciliation-tuple-changed"
	case "startup reconciliation attempt changed":
		return "startup-reconciliation-attempt-changed"
	case "startup reconciliation proof stale":
		return "startup-reconciliation-proof-stale"
	case "startup ready proof missing":
		return "startup-ready-proof-missing"
	case "successful startup effects need reconciliation":
		return "successful-startup-effects-need-reconciliation"
	case "startup replacement bound reached":
		return "startup-replacement-bound-reached"
	case "invalid startup reconciliation":
		return "invalid-startup-reconciliation"
	case "startup grant absent":
		return "startup-grant-absent"
	case "startup recovery authority changed":
		return "startup-recovery-authority-changed"
	case "startup selected package changed":
		return "startup-selected-package-changed"
	case "consumed startup lacks process proof":
		return "consumed-startup-lacks-process-proof"
	case "startup native tuple changed":
		return "startup-native-tuple-changed"
	case "startup process or invocation changed":
		return "startup-process-or-invocation-changed"
	case "startup cgroup identity changed":
		return "startup-cgroup-identity-changed"
	case "startup launch marker unavailable":
		return "startup-launch-marker-unavailable"
	case "startup launch marker invalid":
		return "startup-launch-marker-invalid"
	case "startup launch marker tuple changed":
		return "startup-launch-marker-tuple-changed"
	case "startup changed during readiness proof":
		return "startup-changed-during-readiness-proof"
	case "startup marker exceeds bound":
		return "startup-marker-exceeds-bound"
	case "startup marker malformed":
		return "startup-marker-malformed"
	case "failed startup marker tuple changed":
		return "failed-startup-marker-tuple-changed"
	case "successful startup effects remain unresolved":
		return "successful-startup-effects-remain-unresolved"
	case "startup continuation effects remain unresolved":
		return "startup-continuation-effects-remain-unresolved"
	case "invalid startup workspace namespace":
		return "invalid-startup-workspace-namespace"
	case "invalid startup runtime directory":
		return "invalid-startup-runtime-directory"
	case "startup registered unit tuple changed":
		return "startup-registered-unit-tuple-changed"
	case "startup source service tuple changed":
		return "startup-source-service-tuple-changed"
	case "startup workspace is no longer registered":
		return "startup-workspace-is-no-longer-registered"
	case "incomplete startup unit observation":
		return "incomplete-startup-unit-observation"
	case "unified startup cgroup unavailable":
		return "unified-startup-cgroup-unavailable"
	case "invalid native process identity":
		return "invalid-native-process-identity"
	case "invalid owned startup cgroup":
		return "invalid-owned-startup-cgroup"
	case "startup cgroup still owns processes":
		return "startup-cgroup-still-owns-processes"
	case "startup socket still has a listener":
		return "startup-socket-still-has-a-listener"
	case "startup listener absence is unproved":
		return "startup-listener-absence-is-unproved"
	case "ambiguous startup listeners":
		return "ambiguous-startup-listeners"
	case "startup listener identity unavailable":
		return "startup-listener-identity-unavailable"
	case "consumed process does not own native listener":
		return "consumed-process-does-not-own-native-listener"
	case "consumed process absence unproved":
		return "consumed-process-absence-unproved"
	case "startup boot identity changed":
		return "startup-boot-identity-changed"
	case "startup unit still owns unfinished work":
		return "startup-unit-still-owns-unfinished-work"
	case "maintenance worker invocation changed":
		return "maintenance-worker-invocation-changed"
	case "maintenance worker was superseded":
		return "maintenance-worker-was-superseded"
	case "destination identity required":
		return "destination-identity-required"
	case "destination service changed":
		return "destination-service-changed"
	case "destination epoch unproved":
		return "destination-epoch-unproved"
	case "destination incarnation changed":
		return "destination-incarnation-changed"
	}
	return "identity-or-proof-refused"
}
