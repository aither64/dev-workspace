package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// ForkController is an optional native capability, never creation or mutation
// authority. Callers retain ordinary source and destination session admission.
type ForkController interface {
	CaptureFork(context.Context, agentbackend.ExecutionHandle) (ForkSnapshot, error)
	Fork(context.Context, RuntimeRef, ForkRequest) (ForkProof, error)
	VerifyFork(context.Context, agentbackend.ExecutionHandle, string, ForkOrigin) (ForkProof, error)
}

type NativeForkSnapshot struct {
	SourceFile     string `json:"sourceFile"`
	SourceNativeID string `json:"sourceNativeId"`
	SourceCwd      string `json:"sourceCwd"`
	EntriesDigest  string `json:"entriesDigest"`
	EntryCount     int    `json:"entryCount"`
	LeafID         string `json:"leafId"`
}
type ForkSnapshot struct {
	Native       NativeForkSnapshot
	HistoryRef   string
	Settings     EffectiveSettings
	Instructions string
	ToolPolicy   *ToolPolicyTarget
}
type ForkOrigin struct {
	Source   agentbackend.ExecutionHandle
	Snapshot ForkSnapshot
}
type ForkRequest struct {
	Source      agentbackend.ExecutionHandle
	Snapshot    ForkSnapshot
	Destination CreateRequest
	NativeID    string
}
type ForkProof struct {
	Conversation                       agentbackend.ConversationRef
	Generation, CreationID, HistoryRef string
	Origin                             ForkOrigin
	Persisted                          bool
	Settings                           EffectiveSettings
	InstructionsDigest                 string           `json:",omitempty"`
	ToolPolicy                         *ToolPolicyProof `json:",omitempty"`
}
type nativeForkProof struct {
	NativeForkSnapshot
	DestinationFile     string `json:"destinationFile"`
	DestinationNativeID string `json:"destinationNativeId"`
	Synced              bool   `json:"synced"`
}
type forkOperation struct {
	Destination RuntimeRef
	Request     ForkRequest
}
type forkVerification struct {
	Handle     agentbackend.ExecutionHandle
	CreationID string
	Origin     ForkOrigin
}
type forkRecovery struct {
	Found bool
	Proof ForkProof
}

func validateForkOrigin(origin ForkOrigin) error {
	x := origin.Snapshot
	n := x.Native
	if validateHandle(origin.Source) != nil || x.HistoryRef != historyReference(origin.Source.Conversation) || n.SourceNativeID != origin.Source.Conversation.NativeID || !filepath.IsAbs(n.SourceFile) || filepath.Clean(n.SourceFile) != n.SourceFile || !filepath.IsAbs(n.SourceCwd) || filepath.Clean(n.SourceCwd) != n.SourceCwd || n.EntryCount < 1 || !hexPattern.MatchString(n.EntriesDigest) || n.LeafID == "" || !validString(n.LeafID) || validateSettings(x.Settings) != nil || validateInstructions(x.Instructions) != nil || x.Instructions != "" && x.ToolPolicy == nil {
		return ErrScope
	}
	if x.ToolPolicy != nil && ValidateToolPolicy(*x.ToolPolicy) != nil {
		return ErrScope
	}
	return nil
}
func validateForkRequest(destination RuntimeRef, r ForkRequest) error {
	d := r.Destination
	origin := ForkOrigin{r.Source, r.Snapshot}
	if validateForkOrigin(origin) != nil || destination.Session != d.Session || d.Session == r.Source.Conversation.Session || d.Member != r.Source.Conversation.Member || !memberPattern.MatchString(d.Member) || !hexPattern.MatchString(d.Incarnation) || !creationPattern.MatchString(d.CreationID) || !uuidPattern.MatchString(r.NativeID) || r.NativeID == r.Source.Conversation.NativeID || d.Settings != r.Snapshot.Settings || d.Instructions != r.Snapshot.Instructions || !equalJSON(d.ToolPolicy, r.Snapshot.ToolPolicy) {
		return ErrScope
	}
	return nil
}
func readForkContract(app string) (bool, error) {
	if !storePattern.MatchString(app) {
		return false, ErrScope
	}
	f, err := os.Open(filepath.Join(app, "share/workspace-portal/runtime-contract.json"))
	if err != nil {
		return false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, recordLimit+1))
	if err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if strictDecode(b, &fields, recordLimit) != nil || fields == nil {
		return false, ErrScope
	}
	raw, ok := fields["piFork"]
	if !ok {
		return false, nil
	}
	var n int
	if strictDecode(raw, &n, 32) != nil || n != 1 {
		return false, ErrScope
	}
	return true, nil
}
func (m *Manager) forkAvailable(ref RuntimeRef) error {
	r, err := m.record(ref.Session)
	if err != nil {
		return err
	}
	if r.Runtime != ref {
		return ErrScope
	}
	available, err := m.config.forkContract(r.Start.AppStorePath)
	if err != nil {
		return err
	}
	if !available {
		return ErrUnavailable
	}
	return nil
}
func (m *Manager) CaptureFork(ctx context.Context, h agentbackend.ExecutionHandle) (ForkSnapshot, error) {
	var x ForkSnapshot
	r, err := m.record(h.Conversation.Session)
	if err != nil {
		return x, err
	}
	if r.Runtime.Generation != h.Generation {
		return x, ErrScope
	}
	if err = m.forkAvailable(r.Runtime); err != nil {
		return x, err
	}
	err = m.handleCall(ctx, h, "fork_capture", conversationRequest{}, &x)
	if err == nil {
		err = validateForkOrigin(ForkOrigin{h, x})
	}
	return x, err
}
func (m *Manager) Fork(ctx context.Context, d RuntimeRef, r ForkRequest) (ForkProof, error) {
	var result ForkProof
	if err := validateForkRequest(d, r); err != nil {
		return result, err
	}
	if err := m.forkAvailable(d); err != nil {
		return result, err
	}
	// Reconcile the immutable destination before depending on source liveness or
	// unchanged source history. An incomplete copy never authorizes another ID.
	var recovered forkRecovery
	if err := m.call(ctx, d, "fork_recover", r, &recovered); err != nil {
		return result, err
	}
	if recovered.Found {
		return recovered.Proof, nil
	}
	source, err := m.record(r.Source.Conversation.Session)
	if err != nil {
		return result, err
	}
	if source.Runtime.Generation != r.Source.Generation {
		return result, ErrScope
	}
	if err = m.forkAvailable(source.Runtime); err != nil {
		return result, err
	}
	err = m.call(ctx, source.Runtime, "fork", forkOperation{d, r}, &result)
	return result, err
}
func (m *Manager) VerifyFork(ctx context.Context, h agentbackend.ExecutionHandle, id string, o ForkOrigin) (ForkProof, error) {
	var result ForkProof
	if validateForkOrigin(o) != nil || !creationPattern.MatchString(id) {
		return result, ErrScope
	}
	r, err := m.record(h.Conversation.Session)
	if err != nil {
		return result, err
	}
	if r.Runtime.Generation != h.Generation {
		return result, ErrScope
	}
	if err = m.forkAvailable(r.Runtime); err != nil {
		return result, err
	}
	err = m.call(ctx, r.Runtime, "fork_verify", forkVerification{h, id, o}, &result)
	return result, err
}

func (s *supervisor) forkSnapshotLocked(ctx context.Context, c *conversation) (ForkSnapshot, error) {
	var result ForkSnapshot
	held, err := c.receipts.held()
	if err != nil {
		return result, err
	}
	c.activeMu.Lock()
	unsettled := c.callbackUnknown || c.callbackCount != 0
	c.activeMu.Unlock()
	if held || unsettled || c.record.HeldReason != "" || c.record.PendingSettings != nil || c.record.PendingToolPolicy != nil || c.worker == nil || !c.record.Loaded || !c.record.Persisted {
		return result, ErrHeld
	}
	check := func() (bool, error) { return s.unexpectedChildrenFor(c.record.WorkerPID) }
	if s.childrenCheck != nil {
		check = s.childrenCheck
	}
	if bad, err := check(); err != nil || bad {
		return result, ErrHeld
	}
	idle, err := s.workflowResourcesIdle(ctx)
	if err != nil || !idle {
		return result, ErrHeld
	}
	var snapshot workerSnapshot
	if err = c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		return result, err
	}
	if snapshot.Busy || snapshot.Held || !snapshot.Persisted || snapshot.NativeID != c.record.Conversation.NativeID || snapshot.Model != c.record.Settings.Model || snapshot.Provider != c.record.Settings.Provider || snapshot.Thinking != c.record.Settings.Thinking || snapshot.InstructionsDigest != instructionsDigest(c.record.Instructions) {
		return result, ErrHeld
	}
	if c.record.ToolPolicy != nil {
		var proof ToolPolicyProof
		if c.worker.Call(ctx, "tool_policy_inspect", struct{}{}, &proof) != nil || ValidateToolPolicyProof(proof, *c.record.ToolPolicy, s.ref.Generation) != nil {
			return result, ErrHeld
		}
	}
	if err = c.worker.Call(ctx, "fork_snapshot_v1", struct{}{}, &result.Native); err != nil {
		return result, err
	}
	result.HistoryRef = c.record.HistoryRef
	result.Settings = c.record.Settings
	result.Instructions = c.record.Instructions
	if c.record.ToolPolicy != nil {
		p := cloneToolPolicy(*c.record.ToolPolicy)
		result.ToolPolicy = &p
	}
	if result.Native.SourceFile != c.record.HistoryFile || validateForkOrigin(ForkOrigin{agentbackend.ExecutionHandle{Conversation: c.record.Conversation, Generation: s.ref.Generation}, result}) != nil {
		return ForkSnapshot{}, ErrScope
	}
	return result, nil
}
func (s *supervisor) captureFork(ctx context.Context, h agentbackend.ExecutionHandle) (ForkSnapshot, error) {
	c, err := s.member(h.Conversation)
	if err != nil {
		return ForkSnapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.forkSnapshotLocked(ctx, c)
}

func (s *supervisor) fork(ctx context.Context, op forkOperation) (ForkProof, error) {
	var result ForkProof
	r := op.Request
	if s.metadata.Protocol != 2 || validateForkRequest(op.Destination, r) != nil || s.verifyHandle(r.Source) != nil {
		return result, ErrScope
	}
	// Use the existing short host admission fence as well as the member lock:
	// resources and sibling effects must not enter between quiescence and copy.
	fenced, leave, err := s.hostAdmission.detach(ctx)
	if err != nil {
		return result, err
	}
	defer leave()
	ctx = fenced
	// Native runtime locks are distinct from portal authority admission. Acquire
	// in canonical path order, nonblocking, before any member lock; never upgrade
	// a held shared lock. Destination fork_init does not reacquire either lock.
	sourceDir, err := stateDirectory(s.config.UserStateRoot, s.ref.Session)
	if err != nil {
		return result, err
	}
	destDir, err := stateDirectory(s.config.UserStateRoot, op.Destination.Session)
	if err != nil {
		return result, err
	}
	dirs := []string{sourceDir, destDir}
	sort.Strings(dirs)
	releases := []func(){}
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	for _, dir := range dirs {
		release, err := runtimeLock(dir)
		if err != nil {
			return result, err
		}
		releases = append(releases, release)
	}
	manager := &Manager{config: Config{UserStateRoot: s.config.UserStateRoot, HostStateRoot: filepath.Dir(filepath.Dir(s.verifiedInstanceDirectory)), AuthDirectory: s.config.AuthDirectory, AccountBindings: s.config.AccountBindings, Dial: func(ctx context.Context, path string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	for _, ref := range []RuntimeRef{s.ref, op.Destination} {
		saved, err := InspectRetainedCurrentRuntime(ctx, s.config.UserStateRoot, ref.Session)
		if err != nil {
			return result, err
		}
		if saved.Runtime != ref {
			return result, ErrScope
		}
	}
	c, err := s.member(r.Source.Conversation)
	if err != nil {
		return result, err
	}
	if !c.mu.TryLock() {
		return result, ErrHeld
	}
	defer c.mu.Unlock()
	if err = s.sharedQuiescentLocked(ctx, c, true); err != nil {
		return result, err
	}
	actual, err := s.forkSnapshotLocked(ctx, c)
	if err != nil {
		return result, err
	}
	if !equalJSON(actual, r.Snapshot) {
		return result, ErrConflict
	}
	if err = s.workflowCheck(ctx); err != nil {
		return result, err
	}
	err = manager.callRaw(ctx, op.Destination, "fork_init", r, &result)
	return result, err
}

func (s *supervisor) forkCreate(ctx context.Context, r ForkRequest) (ForkProof, error) {
	if validateForkRequest(s.ref, r) != nil || s.metadata.Protocol != 2 {
		return ForkProof{}, ErrScope
	}
	d := r.Destination
	origin := ForkOrigin{r.Source, r.Snapshot}
	if !bindingAllowed(s.config.AccountBindings, d.Settings) {
		return ForkProof{}, ErrUnavailable
	}
	root := filepath.Join(s.stateRoot, "members", d.Member, d.Incarnation)
	s.mu.Lock()
	var record conversationRecord
	err := readPrivate(filepath.Join(root, "conversation.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		ref := agentbackend.ConversationRef{Session: d.Session, Backend: agentbackend.Pi, Member: d.Member, Incarnation: d.Incarnation, NativeID: r.NativeID}
		record = conversationRecord{Schema: 3, CreationID: d.CreationID, Conversation: ref, HistoryRef: historyReference(ref), Generation: s.ref.Generation, Settings: d.Settings, InitialSettings: d.Settings, Instructions: d.Instructions, Fork: &origin}
		if d.ToolPolicy != nil {
			policy := cloneToolPolicy(*d.ToolPolicy)
			record.PendingToolPolicy = &policy
		}
		err = writePrivate(filepath.Join(root, "conversation.json"), record)
	}
	s.mu.Unlock()
	if err != nil {
		return ForkProof{}, err
	}
	if validateConversationRecord(record, record.Conversation, root) != nil || record.Conversation.NativeID != r.NativeID || record.CreationID != d.CreationID || !equalJSON(record.Fork, &origin) || record.InitialSettings != d.Settings || record.Instructions != d.Instructions {
		return ForkProof{}, ErrConflict
	}
	if _, err = s.loadRetainedPolicy(ctx, agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: s.ref.Generation}, true); err != nil {
		return ForkProof{}, err
	}
	return s.verifyFork(ctx, forkVerification{agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: s.ref.Generation}, d.CreationID, origin})
}
func (s *supervisor) recoverFork(ctx context.Context, r ForkRequest) (forkRecovery, error) {
	if validateForkRequest(s.ref, r) != nil {
		return forkRecovery{}, ErrScope
	}
	root := filepath.Join(s.stateRoot, "members", r.Destination.Member, r.Destination.Incarnation)
	var record conversationRecord
	err := readPrivate(filepath.Join(root, "conversation.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		return forkRecovery{}, nil
	}
	if err != nil {
		return forkRecovery{}, err
	}
	if record.Fork == nil || record.CreationID != r.Destination.CreationID || record.Conversation.NativeID != r.NativeID || !equalJSON(*record.Fork, ForkOrigin{r.Source, r.Snapshot}) {
		return forkRecovery{}, ErrConflict
	}
	entries, err := os.ReadDir(filepath.Join(root, "history"))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(entries) == 0 {
		return forkRecovery{}, nil
	}
	if err != nil {
		return forkRecovery{}, err
	}
	proof, err := s.forkCreate(ctx, r)
	return forkRecovery{Found: err == nil, Proof: proof}, err
}
func (s *supervisor) verifyFork(ctx context.Context, r forkVerification) (ForkProof, error) {
	var result ForkProof
	if validateForkOrigin(r.Origin) != nil || s.verifyHandle(r.Handle) != nil {
		return result, ErrScope
	}
	c, err := s.member(r.Handle.Conversation)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record.Schema != 3 || c.record.Fork == nil || !equalJSON(*c.record.Fork, r.Origin) || c.record.CreationID != r.CreationID || c.worker == nil || !c.record.Persisted {
		return result, ErrHeld
	}
	var native nativeForkProof
	if err = c.worker.Call(ctx, "fork_verify_v1", r.Origin.Snapshot.Native, &native); err != nil {
		return result, err
	}
	if !native.Synced || native.NativeForkSnapshot != r.Origin.Snapshot.Native || native.DestinationFile != c.record.HistoryFile || native.DestinationNativeID != c.record.Conversation.NativeID {
		return result, ErrHeld
	}
	var snapshot workerSnapshot
	if c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot) != nil || snapshot.InstructionsDigest != instructionsDigest(c.record.Instructions) || snapshot.Model != c.record.Settings.Model || snapshot.Provider != c.record.Settings.Provider || snapshot.Thinking != c.record.Settings.Thinking {
		return result, ErrHeld
	}
	result = ForkProof{Conversation: c.record.Conversation, Generation: s.ref.Generation, CreationID: r.CreationID, HistoryRef: c.record.HistoryRef, Origin: *c.record.Fork, Persisted: true, Settings: c.record.Settings, InstructionsDigest: snapshot.InstructionsDigest}
	if c.record.ToolPolicy != nil {
		var proof ToolPolicyProof
		if c.worker.Call(ctx, "tool_policy_inspect", struct{}{}, &proof) != nil || ValidateToolPolicyProof(proof, *c.record.ToolPolicy, s.ref.Generation) != nil {
			return ForkProof{}, ErrHeld
		}
		result.ToolPolicy = &proof
	}
	return result, nil
}

var _ ForkController = (*Manager)(nil)
