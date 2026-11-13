package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type conversationRequest struct {
	Handle                 agentbackend.ExecutionHandle
	Send                   backend.SendRequest
	Settings               EffectiveSettings
	CreationID, HistoryRef string
	Bootstrap              *SubmissionRef
	Before                 *int
}
type conversationRecord struct {
	Schema                              int
	CreationID                          string
	Conversation                        agentbackend.ConversationRef
	HistoryRef, HistoryFile, Generation string
	HeldReason                          string
	Settings                            EffectiveSettings
	PendingSettings                     *EffectiveSettings
	Loaded, Persisted                   bool
	WorkerPID                           int
	WorkerStart                         string
	InitialSettings                     EffectiveSettings
}
type conversation struct {
	mu       sync.Mutex
	activeMu sync.Mutex
	root     string
	record   conversationRecord
	worker   worker
	receipts *receiptEngine
}

func validateSettings(settings EffectiveSettings) error {
	switch settings.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return ErrScope
	}
	if settings.Provider == "" || settings.Model == "" || settings.Revision == 0 || settings.PolicyRevision == "" {
		return ErrScope
	}
	for _, value := range []string{settings.Provider, settings.AccountRef, settings.Model, settings.Thinking, settings.PolicyRevision} {
		if len(value) > 4096 || !validString(value) {
			return ErrScope
		}
	}
	return nil
}

func (s *supervisor) memberRoot(ref agentbackend.ConversationRef) (string, error) {
	if err := s.verifyHandle(agentbackend.ExecutionHandle{Conversation: ref, Generation: s.ref.Generation}); err != nil {
		return "", err
	}
	return filepath.Join(s.stateRoot, "members", ref.Member, ref.Incarnation), nil
}
func (c *conversation) save() error {
	return writePrivate(filepath.Join(c.root, "conversation.json"), c.record)
}
func (s *supervisor) member(ref agentbackend.ConversationRef) (*conversation, error) {
	root, err := s.memberRoot(ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.members[root]; c != nil {
		if c.record.Conversation != ref {
			return nil, ErrConflict
		}
		return c, nil
	}
	var record conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
		return nil, err
	}
	if err := validateConversationRecord(record, ref, root); err != nil {
		return nil, err
	}
	c := &conversation{root: root, record: record, receipts: &receiptEngine{root: filepath.Join(root, "submissions")}}
	s.members[root] = c
	return c, nil
}
func validateConversationRecord(record conversationRecord, ref agentbackend.ConversationRef, root string) error {
	if record.Schema != 1 || record.Conversation != ref || !uuidPattern.MatchString(record.CreationID) || record.HistoryRef != historyReference(ref) || !hexPattern.MatchString(record.Generation) {
		return ErrConflict
	}
	if err := validateSettings(record.InitialSettings); err != nil {
		return err
	}
	if err := validateSettings(record.Settings); err != nil {
		return err
	}
	if record.HistoryFile != "" && (filepath.Dir(record.HistoryFile) != filepath.Join(root, "history") || !filepath.IsAbs(record.HistoryFile)) {
		return ErrScope
	}
	return nil
}

func (s *supervisor) create(ctx context.Context, request CreateRequest) (ConversationState, error) {
	if request.Session != s.ref.Session || !memberPattern.MatchString(request.Member) || !hexPattern.MatchString(request.Incarnation) || !uuidPattern.MatchString(request.CreationID) {
		return ConversationState{}, ErrScope
	}
	if !bindingAllowed(s.config.AccountBindings, request.Settings) {
		return ConversationState{}, ErrUnavailable
	}
	if err := validateSettings(request.Settings); err != nil {
		return ConversationState{}, err
	}
	root := filepath.Join(s.stateRoot, "members", request.Member, request.Incarnation)
	s.mu.Lock()
	var record conversationRecord
	err := readPrivate(filepath.Join(root, "conversation.json"), &record)
	if err == nil {
		if record.CreationID != request.CreationID || record.Conversation.Session != request.Session || record.Conversation.Member != request.Member || record.Conversation.Incarnation != request.Incarnation || record.InitialSettings != request.Settings {
			s.mu.Unlock()
			return ConversationState{}, ErrConflict
		}
	} else if errors.Is(err, os.ErrNotExist) {
		ref := agentbackend.ConversationRef{Session: request.Session, Backend: agentbackend.Pi, Member: request.Member, Incarnation: request.Incarnation, NativeID: newUUID()}
		record = conversationRecord{Schema: 1, CreationID: request.CreationID, Conversation: ref, HistoryRef: historyReference(ref), Generation: s.ref.Generation, Settings: request.Settings, InitialSettings: request.Settings}
		if err = writePrivate(filepath.Join(root, "conversation.json"), record); err != nil {
			s.mu.Unlock()
			return ConversationState{}, err
		}
	} else {
		s.mu.Unlock()
		return ConversationState{}, err
	}
	s.mu.Unlock()
	return s.load(ctx, agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: s.ref.Generation})
}

func (s *supervisor) load(ctx context.Context, handle agentbackend.ExecutionHandle) (ConversationState, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return ConversationState{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !bindingAllowed(s.config.AccountBindings, c.record.Settings) {
		return ConversationState{}, ErrUnavailable
	}
	if c.record.HeldReason != "" {
		return ConversationState{}, ErrHeld
	}
	if c.record.PendingSettings != nil {
		return ConversationState{}, ErrHeld
	}
	if c.worker != nil {
		return s.state(ctx, c)
	}
	held, err := c.receipts.held()
	if err != nil || held {
		return ConversationState{}, ErrHeld
	}
	if c.record.Loaded {
		// A crashed loaded worker may have unobserved tools. Only a never-sent
		// empty mapping with a positively gone original group can be rebuilt.
		entries, readErr := os.ReadDir(c.receipts.root)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return ConversationState{}, readErr
		}
		if c.record.Persisted || len(entries) != 0 || c.record.WorkerPID <= 0 || !processGroupEmpty(c.record.WorkerPID) {
			return ConversationState{}, ErrHeld
		}
	}
	for _, name := range []string{"agent", "history", "submissions"} {
		if err := makePrivateDirectory(filepath.Join(c.root, name)); err != nil {
			return ConversationState{}, err
		}
	}
	cwd, err := sessionDirectory(handle.Conversation.Session)
	if err != nil {
		return ConversationState{}, err
	}
	config := workerConfig{NativeID: handle.Conversation.NativeID, Cwd: cwd, AgentDir: filepath.Join(c.root, "agent"), HistoryDir: filepath.Join(c.root, "history"), AuthDirectory: s.config.AuthDirectory, PiPackage: s.metadata.PiPackage, Settings: c.record.Settings, AccountBindings: s.config.AccountBindings}
	if c.record.Persisted {
		config.HistoryFile = c.record.HistoryFile
		if _, err := os.Stat(config.HistoryFile); err != nil {
			return ConversationState{}, ErrHeld
		}
	} else {
		entries, err := os.ReadDir(c.receipts.root)
		if err != nil || len(entries) != 0 {
			return ConversationState{}, ErrHeld
		}
		if c.record.HistoryFile != "" {
			if _, err := os.Stat(c.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
				return ConversationState{}, ErrHeld
			}
		}
		config.EmptyProven = true
	}
	c.record.Loaded = true
	c.record.Generation = s.ref.Generation
	c.record.WorkerPID = 0
	c.record.WorkerStart = ""
	if err := c.save(); err != nil {
		return ConversationState{}, err
	}
	w, err := s.start(ctx, s.metadata, handle, config, func() {})
	if err != nil {
		return ConversationState{}, err
	}
	c.activeMu.Lock()
	c.worker = w
	c.activeMu.Unlock()
	c.record.WorkerPID, c.record.WorkerStart = w.PID()
	if err := c.save(); err != nil {
		_ = w.Call(ctx, "hold", struct{}{}, nil)
		return ConversationState{}, err
	}
	return s.state(ctx, c)
}

func (s *supervisor) state(ctx context.Context, c *conversation) (ConversationState, error) {
	if c.worker == nil {
		return ConversationState{}, ErrUnavailable
	}
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		return ConversationState{}, err
	}
	if snapshot.NativeID != c.record.Conversation.NativeID || filepath.Dir(snapshot.HistoryFile) != filepath.Join(c.root, "history") {
		return ConversationState{}, ErrScope
	}
	if snapshot.Model != c.record.Settings.Model || snapshot.Provider != c.record.Settings.Provider {
		return ConversationState{}, ErrScope
	}
	c.record.Settings.Thinking = snapshot.Thinking
	c.record.HistoryFile = snapshot.HistoryFile
	c.record.Persisted = snapshot.Persisted
	if err := c.save(); err != nil {
		return ConversationState{}, err
	}
	state := "idle"
	if snapshot.Busy {
		state = "busy"
	}
	held, err := c.receipts.held()
	if err != nil {
		return ConversationState{}, err
	}
	if snapshot.Held || held {
		state = "held"
	}
	return ConversationState{Conversation: c.record.Conversation, Generation: s.ref.Generation, State: state, HistoryRef: c.record.HistoryRef, Persisted: c.record.Persisted, Settings: c.record.Settings}, nil
}

func (s *supervisor) snapshot(ctx context.Context, handle agentbackend.ExecutionHandle, before *int) (workerSnapshot, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return workerSnapshot{}, err
	}
	c.mu.Lock()
	w := c.worker
	c.mu.Unlock()
	if w == nil {
		return workerSnapshot{}, ErrUnavailable
	}
	var snapshot workerSnapshot
	err = w.Call(ctx, "snapshot", struct {
		Before *int `json:"before,omitempty"`
	}{before}, &snapshot)
	return snapshot, err
}

func (s *supervisor) send(ctx context.Context, handle agentbackend.ExecutionHandle, request backend.SendRequest) (SubmissionStatus, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return SubmissionStatus{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record.HeldReason != "" {
		return SubmissionStatus{}, ErrHeld
	}
	if c.worker == nil || c.record.PendingSettings != nil {
		return SubmissionStatus{}, ErrUnavailable
	}
	input := SendInput{Message: request.Message, ActionContext: request.ActionContext, Retry: request.Retry}
	ref := SubmissionRef{Conversation: handle.Conversation, OperationID: request.ClientUserMessageID, Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, c.record.Settings)
	w := c.worker
	status, err := c.receipts.send(ctx, ref, input, c.record.Settings, handle.Generation, func(ctx context.Context) (nativeResult, error) {
		var result nativeResult
		err := w.Call(ctx, "send", map[string]string{"message": request.Message}, &result)
		return result, err
	}, func() {
		holdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Call(holdCtx, "hold", struct{}{}, nil)
	})
	if status.Phase == "Accepted" {
		if _, saveErr := s.state(ctx, c); saveErr != nil {
			return status, saveErr
		}
	}
	return status, err
}

func (s *supervisor) lookup(ref SubmissionRef) (SubmissionStatus, error) {
	c, err := s.member(ref.Conversation)
	if err != nil {
		return SubmissionStatus{}, err
	}
	return c.receipts.lookup(ref)
}

func (s *supervisor) configure(ctx context.Context, handle agentbackend.ExecutionHandle, settings EffectiveSettings) (SettingsResult, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return SettingsResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := SettingsResult{Requested: settings, Effective: c.record.Settings, Outcome: "rejected"}
	if !bindingAllowed(s.config.AccountBindings, c.record.Settings) || !bindingAllowed(s.config.AccountBindings, settings) {
		return result, ErrUnavailable
	}
	if err := validateSettings(settings); err != nil {
		return result, err
	}
	held, err := c.receipts.held()
	if err != nil || held || c.record.HeldReason != "" || c.record.PendingSettings != nil || c.worker == nil {
		return result, ErrHeld
	}

	if settings != c.record.Settings && (settings.Revision != c.record.Settings.Revision+1 || ((settings.Provider != c.record.Settings.Provider) != (settings.AccountRef != c.record.Settings.AccountRef))) {
		return result, nil
	}
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		return result, err
	}
	if snapshot.Model != c.record.Settings.Model || snapshot.Provider != c.record.Settings.Provider || snapshot.Thinking != c.record.Settings.Thinking {
		c.record.HeldReason = "native settings diverged"
		_ = c.save()
		_ = c.worker.Call(ctx, "hold", struct{}{}, nil)
		result.Outcome = "unknown"
		return result, ErrHeld
	}
	if settings == c.record.Settings {
		return SettingsResult{Requested: settings, Effective: settings, Outcome: "applied"}, nil
	}
	if snapshot.Busy || snapshot.Held {
		return result, nil
	}

	check := func() (bool, error) { return s.unexpectedChildrenFor(c.record.WorkerPID) }
	if s.childrenCheck != nil {
		check = s.childrenCheck
	}
	if unknown, err := check(); err != nil || unknown {
		c.record.HeldReason = "unsettled worker descendants before configure"
		_ = c.save()
		_ = c.worker.Call(ctx, "hold", struct{}{}, nil)
		return result, ErrHeld
	}
	c.record.PendingSettings = &settings
	if err := c.save(); err != nil {
		return result, err
	}
	result.Outcome = "unknown"
	if err := c.worker.Call(ctx, "configure", settings, &result); err != nil {
		holdConversation(c)
		return result, err
	}
	switch result.Outcome {
	case "applied":
		effectiveExpected := settings
		effectiveExpected.Thinking = result.Effective.Thinking
		if result.Requested != settings || result.Effective != effectiveExpected || validateSettings(result.Effective) != nil {
			holdConversation(c)
			return SettingsResult{Requested: settings, Effective: c.record.Settings, Outcome: "unknown"}, ErrHeld
		}
		c.record.Settings = result.Effective
	case "rejected":
		if result.Requested != settings || result.Effective != c.record.Settings {
			holdConversation(c)
			return SettingsResult{Requested: settings, Effective: c.record.Settings, Outcome: "unknown"}, ErrHeld
		}
	default:
		holdConversation(c)
		return result, ErrHeld
	}
	c.record.PendingSettings = nil
	if err := c.save(); err != nil {
		holdConversation(c)
		return SettingsResult{Requested: settings, Effective: result.Effective, Outcome: "unknown"}, err
	}
	return result, nil
}

func (s *supervisor) abort(ctx context.Context, handle agentbackend.ExecutionHandle) (StopResult, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return StopResult{}, err
	}
	// Do not wait on the send/configure lock: an uncertain pending witness must
	// be abortable even while its original request is still waiting.
	c.activeMu.Lock()
	w := c.worker
	c.activeMu.Unlock()
	if w == nil {
		return StopResult{Outcome: "unknown"}, ErrUnavailable
	}
	var result struct{ Idle, Held bool }
	if err := w.Call(ctx, "abort", struct{}{}, &result); err != nil {
		return StopResult{Outcome: "unknown"}, err
	}
	return StopResult{Outcome: "requested", Reason: "native idle does not prove descendant tool quiescence"}, nil
}

func (s *supervisor) unload(ctx context.Context, handle agentbackend.ExecutionHandle) (StopResult, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return StopResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	held, err := c.receipts.held()
	if err != nil || held || c.record.HeldReason != "" {
		return StopResult{Outcome: "unknown"}, ErrHeld
	}
	if c.worker == nil {
		if !c.record.Loaded {
			return StopResult{Outcome: "stopped", VerifiedExit: true}, nil
		}
		return StopResult{Outcome: "unknown"}, ErrHeld
	}
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		return StopResult{Outcome: "unknown"}, err
	}
	if snapshot.Busy || snapshot.Held {
		return StopResult{Outcome: "rejected"}, ErrHeld
	}
	check := func() (bool, error) { return s.unexpectedChildrenFor(c.record.WorkerPID) }
	if s.childrenCheck != nil {
		check = s.childrenCheck
	}
	if unknown, err := check(); err != nil || unknown {
		c.record.HeldReason = "unattributed session descendant"
		_ = c.save()
		_ = c.worker.Call(ctx, "hold", struct{}{}, nil)
		return StopResult{Outcome: "unknown", Reason: "unattributed session descendant"}, ErrHeld
	}
	if _, err := s.state(ctx, c); err != nil {
		return StopResult{}, err
	}
	if err := c.worker.Call(ctx, "unload", struct{}{}, nil); err != nil {
		return StopResult{Outcome: "unknown"}, err
	}
	if err := c.worker.Wait(ctx); err != nil {
		return StopResult{Outcome: "unknown"}, err
	}
	pid, _ := c.worker.PID()
	if !processGroupEmpty(pid) {
		c.record.HeldReason = "worker group survivors"
		_ = c.save()
		return StopResult{Outcome: "unknown", Reason: c.record.HeldReason}, ErrHeld
	}
	if unknown, err := check(); err != nil || unknown {
		c.record.HeldReason = "unattributed session descendant after worker exit"
		_ = c.save()
		return StopResult{Outcome: "unknown", Reason: c.record.HeldReason}, ErrHeld
	}
	c.activeMu.Lock()
	c.worker = nil
	c.activeMu.Unlock()
	c.record.Loaded = false
	if err := c.save(); err != nil {
		return StopResult{Outcome: "unknown"}, err
	}
	return StopResult{Outcome: "stopped", VerifiedExit: true}, nil
}

func (s *supervisor) verifyCreation(ctx context.Context, request conversationRequest) (CreationProof, error) {
	c, err := s.member(request.Handle.Conversation)
	if err != nil {
		return CreationProof{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	proof := CreationProof{Conversation: c.record.Conversation, Generation: s.ref.Generation, CreationID: c.record.CreationID, HistoryRef: c.record.HistoryRef}
	if request.CreationID != proof.CreationID || request.HistoryRef != proof.HistoryRef || c.worker == nil || c.record.Generation != request.Handle.Generation {
		return proof, ErrScope
	}
	if request.Bootstrap == nil {
		entries, err := os.ReadDir(c.receipts.root)
		if err != nil || len(entries) != 0 || c.record.Persisted {
			return proof, ErrHeld
		}
		if _, err := os.Stat(c.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
			return proof, ErrHeld
		}
		return proof, nil
	}
	if request.Bootstrap.Conversation != proof.Conversation {
		return proof, ErrScope
	}
	status, err := c.receipts.lookup(*request.Bootstrap)
	if err != nil {
		return proof, err
	}
	if status.Phase != "Accepted" || status.Witness == nil {
		return proof, ErrHeld
	}
	var verified struct{ Verified bool }
	if err := c.worker.Call(ctx, "verify_history", struct {
		Witness *NativeWitness
		Message string
	}{status.Witness, status.Input.Message}, &verified); err != nil || !verified.Verified {
		return proof, ErrHeld
	}
	proof.Persisted = true
	proof.Bootstrap = &status
	return proof, nil
}

func holdConversation(c *conversation) {
	if c.worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.worker.Call(ctx, "hold", struct{}{}, nil)
}
