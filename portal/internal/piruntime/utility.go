package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/aither64/codex-web/conversation/backend"
)

type utilityRequest struct {
	Handle UtilityExecutionHandle
	Grant  UtilityGrant
	Create *UtilityCreateRequest
	Send   *backend.SendRequest
}

func validateUtilityGrant(runtime RuntimeRef, ref UtilityRef, grant UtilityGrant) error {
	if validateUtility(ref, false) != nil || grant.Runtime != runtime || grant.Utility != ref ||
		!operationPattern.MatchString(grant.CreationID) || !operationPattern.MatchString(grant.LaunchOperationID) ||
		validateSettings(grant.Settings) != nil || grant.Input.Message == "" || len(grant.Input.Message) > callbackLimit ||
		!validString(grant.Input.Message) || grant.Input.Retry || grant.Input.ActionContext != "monitor-launch:"+ref.OperationID {
		return ErrScope
	}
	return nil
}

func (m *Manager) withUtility(ctx context.Context, runtime RuntimeRef, ref UtilityRef, action string, run func(UtilityGrant) error) error {
	if m.config.UtilityAuthority == nil {
		return ErrUnavailable
	}
	if runtime.Session != ref.Session || validateUtility(ref, false) != nil {
		return ErrScope
	}
	called := false
	var effectErr error
	err := m.config.UtilityAuthority.WithUtility(ctx, runtime, ref, action, func(grant UtilityGrant) error {
		if called {
			effectErr = ErrConflict
			return effectErr
		}
		called = true
		if effectErr = ctx.Err(); effectErr != nil {
			return effectErr
		}
		if effectErr = validateUtilityGrant(runtime, ref, grant); effectErr != nil {
			return effectErr
		}
		if !bindingAllowed(m.config.AccountBindings, grant.Settings) {
			effectErr = ErrUnavailable
			return effectErr
		}
		effectErr = run(grant)
		return effectErr
	})
	if !called && err == nil {
		return ErrUnavailable
	}
	return errors.Join(err, effectErr)
}

func (m *Manager) utilityRuntime(handle UtilityExecutionHandle) (RuntimeRef, error) {
	if validateUtility(handle.Utility, true) != nil || !hexPattern.MatchString(handle.Generation) {
		return RuntimeRef{}, ErrScope
	}
	record, err := m.record(handle.Utility.Session)
	if err != nil {
		return RuntimeRef{}, err
	}
	if record.Runtime.Generation != handle.Generation {
		return RuntimeRef{}, ErrScope
	}
	return record.Runtime, nil
}

func (m *Manager) CreateUtility(ctx context.Context, runtime RuntimeRef, request UtilityCreateRequest) (UtilityState, error) {
	var result UtilityState
	err := m.withUtility(ctx, runtime, request.Utility, "create", func(grant UtilityGrant) error {
		if request.CreationID != grant.CreationID || request.Settings != grant.Settings {
			return ErrConflict
		}
		return m.call(ctx, runtime, "utility_create", utilityRequest{Grant: grant, Create: &request}, &result)
	})
	if err == nil {
		reserved := result.Utility
		reserved.NativeID = request.Utility.NativeID
		if reserved != request.Utility || validateUtility(result.Utility, true) != nil || result.Generation != runtime.Generation || result.Settings != request.Settings {
			return UtilityState{}, ErrScope
		}
	}
	return result, err
}

func (m *Manager) utilityEffect(ctx context.Context, handle UtilityExecutionHandle, action, operation string, request utilityRequest, out any) error {
	runtime, err := m.utilityRuntime(handle)
	if err != nil {
		return err
	}
	return m.withUtility(ctx, runtime, handle.Utility, action, func(grant UtilityGrant) error {
		request.Handle, request.Grant = handle, grant
		return m.call(ctx, runtime, operation, request, out)
	})
}

func (m *Manager) LoadUtility(ctx context.Context, handle UtilityExecutionHandle) (UtilityState, error) {
	var result UtilityState
	err := m.utilityEffect(ctx, handle, "load", "utility_load", utilityRequest{}, &result)
	if err == nil && (result.Utility != handle.Utility || result.Generation != handle.Generation) {
		return UtilityState{}, ErrScope
	}
	return result, err
}

func utilitySubmission(grant UtilityGrant) UtilitySubmissionRef {
	ref := UtilitySubmissionRef{Utility: grant.Utility, OperationID: grant.LaunchOperationID, Action: "send"}
	ref.Digest = UtilitySubmissionDigest(ref, grant.Input, grant.Settings)
	return ref
}

func (m *Manager) BindUtility(ctx context.Context, handle UtilityExecutionHandle) (backend.Operations, error) {
	var original UtilitySubmissionRef
	runtime, err := m.utilityRuntime(handle)
	if err != nil {
		return backend.Operations{}, err
	}
	err = m.withUtility(ctx, runtime, handle.Utility, "bind", func(grant UtilityGrant) error {
		if err := m.call(ctx, runtime, "utility_bind", utilityRequest{Handle: handle, Grant: grant}, &original); err != nil {
			return err
		}
		if original != utilitySubmission(grant) {
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		return backend.Operations{}, err
	}
	if original.Utility != handle.Utility || !operationPattern.MatchString(original.OperationID) || original.Action != "send" || !hexPattern.MatchString(original.Digest) {
		return backend.Operations{}, ErrScope
	}
	adapter := &utilityAdapter{manager: m, handle: handle, original: original}
	// Interrupt has no authority action in this contract. UnloadUtility is a
	// separately admitted effect and is never an alias for interruption.
	return backend.Operations{Verify: adapter, Read: adapter, Send: adapter}, nil
}

func (m *Manager) utilityRead(ctx context.Context, ref UtilitySubmissionRef, read func(RuntimeRef, *conversation) error) error {
	if m.config.UtilityReadAuthority == nil {
		return ErrUnavailable
	}
	if validateUtility(ref.Utility, true) != nil || !operationPattern.MatchString(ref.OperationID) || ref.Action != "send" || !hexPattern.MatchString(ref.Digest) {
		return ErrScope
	}
	record, err := m.record(ref.Utility.Session)
	if err != nil {
		return err
	}
	called := false
	var readErr error
	err = m.config.UtilityReadAuthority.WithUtilityRead(ctx, record.Runtime, ref, func() error {
		if called {
			readErr = ErrConflict
			return readErr
		}
		called = true
		if readErr = ctx.Err(); readErr != nil {
			return readErr
		}
		directory, err := stateDirectory(m.config.UserStateRoot, ref.Utility.Session)
		if err != nil {
			readErr = err
			return err
		}
		root := filepath.Join(directory, "utilities", ref.Utility.OperationID, ref.Utility.Incarnation)
		var mapping conversationRecord
		if readErr = readPrivate(filepath.Join(root, "conversation.json"), &mapping); readErr != nil {
			return readErr
		}
		if readErr = validateUtilityRecord(mapping, ref.Utility, root); readErr != nil {
			return readErr
		}
		c := &conversation{root: root, record: mapping, receipts: &receiptEngine{root: filepath.Join(root, "submissions"), ownerKind: "utility"}}
		readErr = read(record.Runtime, c)
		return readErr
	})
	if !called && err == nil {
		return ErrUnavailable
	}
	return errors.Join(err, readErr)
}

func (m *Manager) LookupUtility(ctx context.Context, ref UtilitySubmissionRef) (UtilitySubmissionStatus, error) {
	var result UtilitySubmissionStatus
	err := m.utilityRead(ctx, ref, func(_ RuntimeRef, c *conversation) error {
		var err error
		result, err = c.receipts.lookupUtility(ref)
		return err
	})
	return result, err
}

func (m *Manager) UnloadUtility(ctx context.Context, handle UtilityExecutionHandle) (StopResult, error) {
	var result StopResult
	err := m.utilityEffect(ctx, handle, "unload", "utility_unload", utilityRequest{}, &result)
	return result, err
}

func (s *supervisor) utilityRoot(ref UtilityRef) (string, error) {
	if validateUtility(ref, false) != nil || ref.Session != s.ref.Session {
		return "", ErrScope
	}
	return filepath.Join(s.stateRoot, "utilities", ref.OperationID, ref.Incarnation), nil
}
func (s *supervisor) utility(ref UtilityRef) (*conversation, error) {
	if validateUtility(ref, true) != nil {
		return nil, ErrScope
	}
	root, err := s.utilityRoot(ref)
	if err != nil {
		return nil, err
	}
	return s.ownedConversation(root, "utility", func(record conversationRecord) error { return validateUtilityRecord(record, ref, root) })
}

func (s *supervisor) createUtility(ctx context.Context, request UtilityCreateRequest) (UtilityState, error) {
	root, err := s.utilityRoot(request.Utility)
	if err != nil {
		return UtilityState{}, err
	}
	if !operationPattern.MatchString(request.CreationID) || validateSettings(request.Settings) != nil || !bindingAllowed(s.config.AccountBindings, request.Settings) {
		return UtilityState{}, ErrScope
	}
	s.mu.Lock()
	var record conversationRecord
	err = readPrivate(filepath.Join(root, "conversation.json"), &record)
	if errors.Is(err, os.ErrNotExist) {
		if request.Utility.NativeID != "" {
			s.mu.Unlock()
			return UtilityState{}, ErrHeld
		}
		ref := request.Utility
		ref.NativeID = newUUID()
		record = conversationRecord{Schema: 2, OwnerKind: "utility", Utility: &ref, CreationID: request.CreationID, Generation: s.ref.Generation, Settings: request.Settings, InitialSettings: request.Settings}
		err = writePrivate(filepath.Join(root, "conversation.json"), record)
	} else if err == nil {
		if record.Utility == nil || validateUtilityRecord(record, *record.Utility, root) != nil || record.CreationID != request.CreationID || record.InitialSettings != request.Settings {
			s.mu.Unlock()
			return UtilityState{}, ErrConflict
		}
		reserved := *record.Utility
		reserved.NativeID = request.Utility.NativeID
		if reserved != request.Utility {
			s.mu.Unlock()
			return UtilityState{}, ErrConflict
		}
	}
	s.mu.Unlock()
	if err != nil {
		return UtilityState{}, err
	}
	return s.loadUtility(ctx, UtilityExecutionHandle{Utility: *record.Utility, Generation: s.ref.Generation})
}

func (s *supervisor) loadUtility(ctx context.Context, handle UtilityExecutionHandle) (UtilityState, error) {
	c, err := s.utility(handle.Utility)
	if err != nil {
		return UtilityState{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := s.loadOwned(ctx, c)
	return utilityState(c, state), err
}

func (s *supervisor) invokeUtility(ctx context.Context, operation string, request utilityRequest) (any, error) {
	grant := request.Grant
	if validateUtilityGrant(s.ref, grant.Utility, grant) != nil {
		return nil, ErrScope
	}
	if operation == "utility_create" {
		if request.Create == nil || request.Create.Utility != grant.Utility || request.Create.CreationID != grant.CreationID || request.Create.Settings != grant.Settings {
			return nil, ErrConflict
		}
		return s.createUtility(ctx, *request.Create)
	}
	if request.Handle.Utility != grant.Utility || request.Handle.Generation != s.ref.Generation || validateUtility(grant.Utility, true) != nil {
		return nil, ErrScope
	}
	c, err := s.utility(grant.Utility)
	if err != nil {
		return nil, err
	}
	if c.record.CreationID != grant.CreationID || c.record.InitialSettings != grant.Settings {
		return nil, ErrConflict
	}
	switch operation {
	case "utility_load":
		return s.loadUtility(ctx, request.Handle)
	case "utility_unload":
		return s.unloadOwned(ctx, c)
	case "utility_bind":
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.worker == nil || c.record.Generation != request.Handle.Generation {
			return nil, ErrUnavailable
		}
		return utilitySubmission(grant), nil
	case "utility_send":
		if request.Send == nil || *request.Send != (backend.SendRequest{Message: grant.Input.Message, ClientUserMessageID: grant.LaunchOperationID, ActionContext: grant.Input.ActionContext, Retry: grant.Input.Retry}) {
			return nil, ErrConflict
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		ref := utilitySubmission(grant)
		status, err := s.sendOwned(ctx, c, receiptReference{utility: &ref}, grant.Input)
		return status.utility(), err
	default:
		return nil, ErrUnavailable
	}
}

var _ UtilityController = (*Manager)(nil)
