package piruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type conversationRequest struct {
	Handle                 agentbackend.ExecutionHandle
	Send                   backend.SendRequest
	Settings               EffectiveSettings
	CreationID, HistoryRef string
	Bootstrap              *SubmissionRef
	Origin                 *NativeOriginRef
	Before                 *int
}
type conversationRecord struct {
	Schema                  int
	CreationID              string
	Instructions            string                       `json:",omitempty"`
	Conversation            agentbackend.ConversationRef `json:",omitzero"`
	HistoryRef              string                       `json:",omitempty"`
	HistoryFile, Generation string
	HeldReason              string
	Settings                EffectiveSettings
	PendingSettings         *EffectiveSettings
	Loaded, Persisted       bool
	WorkerPID               int
	WorkerStart             string
	InitialSettings         EffectiveSettings
	OwnerKind               string            `json:",omitempty"`
	Utility                 *UtilityRef       `json:",omitempty"`
	ToolPolicy              *ToolPolicyState  `json:",omitempty"`
	PendingToolPolicy       *ToolPolicyTarget `json:",omitempty"`
}
type conversation struct {
	mu              sync.Mutex
	activeMu        sync.Mutex
	root            string
	record          conversationRecord
	worker          worker
	receipts        *receiptEngine
	activeRef       *receiptReference
	callbackUnknown bool
	hostTools       []string
	callbackCount   int
}

func validateInstructions(value string) error {
	if len(value) > 4096 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return ErrScope
	}
	return nil
}

// A present empty key is not legacy: old strict readers reject it too. Keep
// the canonical presence rule in the owning codec, before inventory projection.
func decodeInstructions(data []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return ErrScope
	}
	if raw, exists := fields["Instructions"]; exists {
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" || validateInstructions(value) != nil {
			return ErrScope
		}
	}
	return nil
}

func (record *conversationRecord) UnmarshalJSON(data []byte) error {
	type plain conversationRecord
	var value plain
	if decodeInstructions(data) != nil || strictDecode(data, &value, recordLimit) != nil {
		return ErrScope
	}
	if value.Instructions != "" && (value.Schema != 2 || value.OwnerKind != "" || value.Utility != nil) {
		return ErrScope
	}
	*record = conversationRecord(value)
	return nil
}

func (request *CreateRequest) UnmarshalJSON(data []byte) error {
	type plain CreateRequest
	var value plain
	if decodeInstructions(data) != nil || strictDecode(data, &value, recordLimit) != nil {
		return ErrScope
	}
	*request = CreateRequest(value)
	return nil
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
	return s.ownedConversation(root, "retained", func(record conversationRecord) error {
		return validateConversationRecord(record, ref, root)
	})
}
func (s *supervisor) ownedConversation(root, ownerKind string, validate func(conversationRecord) error) (*conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.members[root]; c != nil {
		if err := validate(c.record); err != nil {
			return nil, err
		}
		return c, nil
	}
	var record conversationRecord
	if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
		return nil, err
	}
	if err := validate(record); err != nil {
		return nil, err
	}
	c := &conversation{root: root, record: record, receipts: &receiptEngine{root: filepath.Join(root, "submissions"), ownerKind: ownerKind}}
	s.members[root] = c
	return c, nil
}

func validateConversationRecord(record conversationRecord, ref agentbackend.ConversationRef, root string) error {
	if (record.Schema != 1 && record.Schema != 2) || record.OwnerKind != "" || record.Utility != nil || record.Conversation != ref || !creationPattern.MatchString(record.CreationID) || record.HistoryRef != historyReference(ref) || !hexPattern.MatchString(record.Generation) {
		return ErrConflict
	}
	if validateInstructions(record.Instructions) != nil || record.Instructions != "" && record.Schema != 2 {
		return ErrScope
	}
	if err := validateRetainedToolPolicyRecord(record); err != nil {
		return err
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
	if validateInstructions(request.Instructions) != nil || request.Instructions != "" && request.ToolPolicy == nil {
		return ConversationState{}, ErrScope
	}
	if request.Session != s.ref.Session || !memberPattern.MatchString(request.Member) || !hexPattern.MatchString(request.Incarnation) || !creationPattern.MatchString(request.CreationID) {
		return ConversationState{}, ErrScope
	}
	if !bindingAllowed(s.config.AccountBindings, request.Settings) {
		return ConversationState{}, ErrUnavailable
	}
	if err := validateSettings(request.Settings); err != nil {
		return ConversationState{}, err
	}
	if request.ToolPolicy != nil && (request.ToolPolicy.Revision != 1 || ValidateToolPolicy(*request.ToolPolicy) != nil) {
		return ConversationState{}, ErrScope
	}
	freshPolicy := false
	root := filepath.Join(s.stateRoot, "members", request.Member, request.Incarnation)
	s.mu.Lock()
	if err := s.workflowCheck(ctx); err != nil {
		s.mu.Unlock()
		return ConversationState{}, err
	}
	var record conversationRecord
	err := readPrivate(filepath.Join(root, "conversation.json"), &record)
	if err == nil {
		if err := validateConversationRecord(record, record.Conversation, root); err != nil {
			s.mu.Unlock()
			return ConversationState{}, err
		}
		if err := validateInitialToolPolicyReplay(record.Schema, record.ToolPolicy, record.PendingToolPolicy, request.ToolPolicy); err != nil {
			s.mu.Unlock()
			return ConversationState{}, err
		}
		if record.CreationID != request.CreationID || record.Conversation.Session != request.Session || record.Conversation.Member != request.Member || record.Conversation.Incarnation != request.Incarnation || record.InitialSettings != request.Settings || record.Instructions != request.Instructions {
			s.mu.Unlock()
			return ConversationState{}, ErrConflict
		}
	} else if errors.Is(err, os.ErrNotExist) {
		ref := agentbackend.ConversationRef{Session: request.Session, Backend: agentbackend.Pi, Member: request.Member, Incarnation: request.Incarnation, NativeID: newUUID()}
		record = conversationRecord{Schema: 1, CreationID: request.CreationID, Instructions: request.Instructions, Conversation: ref, HistoryRef: historyReference(ref), Generation: s.ref.Generation, Settings: request.Settings, InitialSettings: request.Settings}
		if request.ToolPolicy != nil {
			target := *request.ToolPolicy
			record.Schema, record.PendingToolPolicy = 2, &target
			freshPolicy = true
		}
		if err = writePrivate(filepath.Join(root, "conversation.json"), record); err != nil {
			s.mu.Unlock()
			return ConversationState{}, err
		}
	} else {
		s.mu.Unlock()
		return ConversationState{}, err
	}
	s.mu.Unlock()
	return s.loadRetainedPolicy(ctx, agentbackend.ExecutionHandle{Conversation: record.Conversation, Generation: s.ref.Generation}, freshPolicy)
}

func (s *supervisor) load(ctx context.Context, handle agentbackend.ExecutionHandle) (ConversationState, error) {
	return s.loadRetainedPolicy(ctx, handle, false)
}
func (s *supervisor) loadRetainedPolicy(ctx context.Context, handle agentbackend.ExecutionHandle, freshPolicy bool) (ConversationState, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return ConversationState{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := s.loadOwnedPolicy(ctx, c, freshPolicy)
	return retainedState(c, state), err
}
func (s *supervisor) loadOwned(ctx context.Context, c *conversation) (ownedConversationState, error) {
	return s.loadOwnedPolicy(ctx, c, false)
}
func (s *supervisor) loadOwnedPolicy(ctx context.Context, c *conversation, freshPolicy bool) (ownedConversationState, error) {
	if err := s.workflowCheck(ctx); err != nil {
		return ownedConversationState{}, err
	}
	identity := c.identity(s.ref.Generation)
	subject := identity.subject()
	if !bindingAllowed(s.config.AccountBindings, c.record.Settings) {
		return ownedConversationState{}, ErrUnavailable
	}
	if c.record.HeldReason != "" {
		return ownedConversationState{}, ErrHeld
	}
	if c.record.PendingSettings != nil {
		return ownedConversationState{}, ErrHeld
	}
	policy := c.record.ToolPolicy
	if c.record.PendingToolPolicy != nil {
		if !freshPolicy || c.record.ToolPolicy != nil || c.record.PendingToolPolicy.Revision != 1 || c.record.Loaded || c.record.Persisted || c.worker != nil {
			return ownedConversationState{}, ErrHeld
		}
		policy = c.record.PendingToolPolicy
	}
	if c.worker != nil {
		if policy != nil {
			var proof ToolPolicyProof
			if err := c.worker.Call(ctx, "tool_policy_inspect", struct{}{}, &proof); err != nil || ValidateToolPolicyProof(proof, *policy, s.ref.Generation) != nil {
				return ownedConversationState{}, ErrHeld
			}
		}
		return s.observeState(ctx, c)
	}
	held, err := c.receipts.held()
	if err != nil || held {
		return ownedConversationState{}, ErrHeld
	}
	if c.record.Loaded {
		// A crashed loaded worker may have unobserved tools. Only a never-sent
		// empty mapping with a positively gone original group can be rebuilt.
		entries, readErr := os.ReadDir(c.receipts.root)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return ownedConversationState{}, readErr
		}
		if c.record.Persisted || len(entries) != 0 || c.record.WorkerPID <= 0 || !processGroupEmpty(c.record.WorkerPID) {
			return ownedConversationState{}, ErrHeld
		}
	}
	for _, name := range []string{"agent", "history", "submissions"} {
		if err := makePrivateDirectory(filepath.Join(c.root, name)); err != nil {
			return ownedConversationState{}, err
		}
	}
	cwd, err := sessionDirectory(s.ref.Session)
	if err != nil {
		return ownedConversationState{}, err
	}
	config := workerConfig{Instructions: c.record.Instructions, Generation: s.ref.Generation, ToolPolicy: policy, NativeID: identity.nativeID(), Cwd: cwd, AgentDir: filepath.Join(c.root, "agent"), HistoryDir: filepath.Join(c.root, "history"), AuthDirectory: s.config.AuthDirectory, PiPackage: s.metadata.PiPackage, Settings: c.record.Settings, AccountBindings: s.config.AccountBindings}
	c.hostTools = s.availableHostTools(subject.Kind, policy)
	config.HostTools = append([]string{}, c.hostTools...)
	config.CallbackSubject = &subject
	config.onHostCall = func(ctx context.Context, call nativeHostCall) (HostReply, error) {
		c.activeMu.Lock()
		c.callbackCount++
		c.activeMu.Unlock()
		s.workflowChanged()
		defer func() { c.activeMu.Lock(); c.callbackCount--; c.activeMu.Unlock(); s.workflowChanged() }()
		return s.ownedHostCall(ctx, c, identity, c.hostTools, call)
	}
	if c.record.Persisted {
		config.HistoryFile = c.record.HistoryFile
		if _, err := os.Stat(config.HistoryFile); err != nil {
			return ownedConversationState{}, ErrHeld
		}
	} else {
		entries, err := os.ReadDir(c.receipts.root)
		if err != nil || len(entries) != 0 {
			return ownedConversationState{}, ErrHeld
		}
		if c.record.HistoryFile != "" {
			if _, err := os.Stat(c.record.HistoryFile); !errors.Is(err, os.ErrNotExist) {
				return ownedConversationState{}, ErrHeld
			}
		}
		config.EmptyProven = true
	}
	c.record.Loaded = true
	c.record.Generation = s.ref.Generation
	c.record.WorkerPID = 0
	c.record.WorkerStart = ""
	if err := c.save(); err != nil {
		return ownedConversationState{}, err
	}
	if err := s.workflowCheck(ctx); err != nil {
		return ownedConversationState{}, err
	}
	var w worker
	if identity.OwnerKind == "utility" {
		w, err = startOwnedWorker(ctx, s.metadata, identity, config, s.workflowChanged)
	} else {
		w, err = s.start(ctx, s.metadata, agentbackend.ExecutionHandle{Conversation: identity.Conversation, Generation: identity.Generation}, config, s.workflowChanged)
	}
	if err != nil {
		return ownedConversationState{}, err
	}
	c.activeMu.Lock()
	c.worker = w
	c.activeMu.Unlock()
	previousPolicy, previousPending := c.record.ToolPolicy, c.record.PendingToolPolicy
	if policy != nil {
		var proof ToolPolicyProof
		if err := w.Call(ctx, "tool_policy_inspect", struct{}{}, &proof); err != nil || ValidateToolPolicyProof(proof, *policy, s.ref.Generation) != nil {
			c.record.HeldReason = "initial tool policy unresolved"
			_ = c.save()
			holdConversation(c)
			return ownedConversationState{}, ErrHeld
		}
		target := *policy
		c.record.ToolPolicy, c.record.PendingToolPolicy = &target, nil
	}
	c.record.WorkerPID, c.record.WorkerStart = w.PID()
	if err := c.save(); err != nil {
		c.record.ToolPolicy, c.record.PendingToolPolicy = previousPolicy, previousPending
		c.record.HeldReason = "worker setup durable completion unresolved"
		_ = w.Call(ctx, "hold", struct{}{}, nil)
		return ownedConversationState{}, err
	}
	return s.observeState(ctx, c)
}

func (s *supervisor) state(ctx context.Context, c *conversation) (ConversationState, error) {
	state, err := s.observeState(ctx, c)
	return retainedState(c, state), err
}
func (s *supervisor) observeState(ctx context.Context, c *conversation) (ownedConversationState, error) {
	if c.worker == nil {
		return ownedConversationState{}, ErrUnavailable
	}
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		return ownedConversationState{}, err
	}
	if snapshot.NativeID != c.identity(s.ref.Generation).nativeID() || filepath.Dir(snapshot.HistoryFile) != filepath.Join(c.root, "history") {
		return ownedConversationState{}, ErrScope
	}
	if snapshot.Model != c.record.Settings.Model || snapshot.Provider != c.record.Settings.Provider {
		return ownedConversationState{}, ErrScope
	}
	c.record.Settings.Thinking = snapshot.Thinking
	c.record.HistoryFile = snapshot.HistoryFile
	c.record.Persisted = snapshot.Persisted
	if err := c.save(); err != nil {
		return ownedConversationState{}, err
	}
	state := "idle"
	if snapshot.Busy {
		state = "busy"
	}
	held, err := c.receipts.held()
	if err != nil {
		return ownedConversationState{}, err
	}
	c.activeMu.Lock()
	callbacks := c.callbackCount
	callbackUnknown := c.callbackUnknown
	c.activeMu.Unlock()
	if callbacks > 0 {
		state = "busy"
	}
	if snapshot.Held || held || callbackUnknown || c.record.HeldReason != "" || c.record.PendingToolPolicy != nil || c.record.PendingSettings != nil {
		state = "held"
	}
	return ownedConversationState{Generation: s.ref.Generation, State: state, Persisted: c.record.Persisted, Settings: c.record.Settings}, nil
}

func (s *supervisor) snapshot(ctx context.Context, handle agentbackend.ExecutionHandle, before *int) (workerSnapshot, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return workerSnapshot{}, err
	}
	c.mu.Lock()
	w := c.worker
	hostHeld := c.record.HeldReason != "" || c.record.PendingSettings != nil || c.record.PendingToolPolicy != nil
	c.mu.Unlock()
	if w == nil {
		return workerSnapshot{}, ErrUnavailable
	}
	var snapshot workerSnapshot
	err = w.Call(ctx, "snapshot", struct {
		Before *int `json:"before,omitempty"`
	}{before}, &snapshot)
	c.activeMu.Lock()
	snapshot.Busy = snapshot.Busy || c.callbackCount > 0
	snapshot.Held = snapshot.Held || c.callbackUnknown || hostHeld
	c.activeMu.Unlock()
	return snapshot, err
}

func (s *supervisor) send(ctx context.Context, handle agentbackend.ExecutionHandle, request backend.SendRequest) (SubmissionStatus, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return SubmissionStatus{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	input := SendInput{Message: request.Message, ActionContext: request.ActionContext, Retry: request.Retry}
	ref := SubmissionRef{Conversation: handle.Conversation, OperationID: request.ClientUserMessageID, Action: "send"}
	ref.Digest = SubmissionDigest(ref, input, c.record.Settings)
	status, err := s.sendOwned(ctx, c, receiptReference{retained: &ref}, input)
	return status.retained(), err
}
func (s *supervisor) sendOwned(ctx context.Context, c *conversation, ref receiptReference, input SendInput) (ownedReceipt, error) {
	if err := s.workflowCheck(ctx); err != nil {
		return ownedReceipt{}, err
	}
	if c.record.HeldReason != "" {
		return ownedReceipt{}, ErrHeld
	}
	c.activeMu.Lock()
	callbackUnknown := c.callbackUnknown
	c.activeMu.Unlock()
	if callbackUnknown {
		return ownedReceipt{}, ErrHeld
	}
	s.callbackMu.Lock()
	attachment := s.attachment
	toolsReady := len(c.hostTools) == 0
	if attachment != nil {
		toolsReady = true
		for _, name := range c.hostTools {
			if !slices.Contains(attachment.tools, name) {
				toolsReady = false
			}
		}
	}
	s.callbackMu.Unlock()
	if !toolsReady {
		return ownedReceipt{}, ErrUnavailable
	}
	if c.worker == nil || c.record.PendingSettings != nil || c.record.PendingToolPolicy != nil {
		return ownedReceipt{}, ErrUnavailable
	}
	if c.record.ToolPolicy != nil {
		var proof ToolPolicyProof
		if err := c.worker.Call(ctx, "tool_policy_inspect", struct{}{}, &proof); err != nil || ValidateToolPolicyProof(proof, *c.record.ToolPolicy, s.ref.Generation) != nil {
			return ownedReceipt{}, ErrHeld
		}
	}
	w := c.worker
	status, err := c.receipts.sendOwned(ctx, ref, input, c.record.Settings, s.ref.Generation, func(ctx context.Context) (nativeResult, error) {
		if err := s.workflowCheck(ctx); err != nil {
			return nativeResult{}, err
		}
		c.activeMu.Lock()
		c.activeRef = &ref
		c.activeMu.Unlock()
		var result nativeResult
		err := w.Call(ctx, "send", map[string]string{"message": input.Message, "inputOperationID": ref.id()}, &result)
		return result, err
	}, func() {
		holdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Call(holdCtx, "hold", struct{}{}, nil)
	})
	if status.Phase == "Accepted" {
		if _, saveErr := s.observeState(ctx, c); saveErr != nil {
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

func (s *supervisor) inspectOrigin(ctx context.Context, handle agentbackend.ExecutionHandle, origin NativeOriginRef) (NativeOriginObservation, error) {
	var result NativeOriginObservation
	if err := validateNativeOrigin(origin); err != nil {
		return result, err
	}
	c, err := s.member(handle.Conversation)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.worker == nil || c.record.Generation != handle.Generation || !c.record.Persisted {
		return result, ErrUnavailable
	}
	var observed struct {
		RootEntryID string
		Arguments   json.RawMessage
	}
	if err := c.worker.Call(ctx, "inspect_origin", origin, &observed); err != nil {
		return result, err
	}
	token, err := LinearSelectionToken(CallbackSubject{Kind: "conversation", Conversation: &handle}, observed.RootEntryID)
	if err != nil || token != origin.SelectionToken || len(observed.Arguments) > callbackLimit || !json.Valid(observed.Arguments) {
		return result, ErrScope
	}
	result = NativeOriginObservation{Handle: handle, Origin: origin, Arguments: append(json.RawMessage(nil), observed.Arguments...)}
	return result, nil
}

func (s *supervisor) configure(ctx context.Context, handle agentbackend.ExecutionHandle, settings EffectiveSettings) (SettingsResult, error) {
	c, err := s.member(handle.Conversation)
	if err != nil {
		return SettingsResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := SettingsResult{Requested: settings, Effective: c.record.Settings, Outcome: "rejected"}
	if err := s.workflowCheck(ctx); err != nil {
		return result, err
	}
	if !bindingAllowed(s.config.AccountBindings, c.record.Settings) || !bindingAllowed(s.config.AccountBindings, settings) {
		return result, ErrUnavailable
	}
	if err := validateSettings(settings); err != nil {
		return result, err
	}
	c.activeMu.Lock()
	callbacksHeld := c.callbackUnknown || c.callbackCount != 0
	c.activeMu.Unlock()
	held, err := c.receipts.held()
	if err != nil || held || callbacksHeld || c.record.HeldReason != "" || c.record.PendingSettings != nil || c.record.PendingToolPolicy != nil || c.worker == nil {
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
	if err := s.workflowCheck(ctx); err != nil {
		return result, err
	}
	c.record.PendingSettings = &settings
	if err := c.save(); err != nil {
		return result, err
	}
	result.Outcome = "unknown"
	if err := s.workflowCheck(ctx); err != nil {
		return result, err
	}
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
	if err := s.workflowCheck(ctx); err != nil {
		return StopResult{}, err
	}
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
	return s.unloadOwned(ctx, c)
}
func (s *supervisor) unloadOwned(ctx context.Context, c *conversation) (StopResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.workflowCheck(ctx); err != nil {
		return StopResult{}, err
	}
	c.activeMu.Lock()
	callbacksHeld := c.callbackUnknown || c.callbackCount != 0
	c.activeMu.Unlock()
	held, err := c.receipts.held()
	if err != nil || held || callbacksHeld || c.record.HeldReason != "" || c.record.PendingToolPolicy != nil || c.record.PendingSettings != nil {
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
	if _, err := s.observeState(ctx, c); err != nil {
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
	if c.record.Instructions != "" {
		var snapshot workerSnapshot
		if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil || snapshot.InstructionsDigest != instructionsDigest(c.record.Instructions) {
			return proof, ErrHeld
		}
		proof.InstructionsDigest = snapshot.InstructionsDigest
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

func validateUtilityRecord(record conversationRecord, ref UtilityRef, root string) error {
	if record.Instructions != "" {
		return ErrScope
	}
	if record.Schema != 2 || record.OwnerKind != "utility" || record.Utility == nil || *record.Utility != ref || record.Conversation != (agentbackend.ConversationRef{}) || record.HistoryRef != "" || record.ToolPolicy != nil || record.PendingToolPolicy != nil || validateUtility(ref, true) != nil || !operationPattern.MatchString(record.CreationID) || !hexPattern.MatchString(record.Generation) {
		return ErrConflict
	}
	if validateSettings(record.Settings) != nil || validateSettings(record.InitialSettings) != nil {
		return ErrScope
	}
	if record.HistoryFile != "" && (filepath.Dir(record.HistoryFile) != filepath.Join(root, "history") || !filepath.IsAbs(record.HistoryFile)) {
		return ErrScope
	}
	return nil
}

func validateRetainedToolPolicyRecord(record conversationRecord) error {
	if record.Schema == 1 && (record.ToolPolicy != nil || record.PendingToolPolicy != nil) {
		return ErrConflict
	}
	for _, target := range []*ToolPolicyTarget{record.ToolPolicy, record.PendingToolPolicy} {
		if target != nil && ValidateToolPolicy(*target) != nil {
			return ErrConflict
		}
	}
	if record.PendingToolPolicy != nil {
		if record.ToolPolicy == nil {
			if record.PendingToolPolicy.Revision != 1 {
				return ErrConflict
			}
		} else if record.ToolPolicy.Revision == MaxToolPolicyRevision || record.PendingToolPolicy.Revision != record.ToolPolicy.Revision+1 || !slices.Equal(record.ToolPolicy.RegisteredCeiling, record.PendingToolPolicy.RegisteredCeiling) {
			return ErrConflict
		}
	}
	return nil
}
