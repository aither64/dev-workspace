package piruntime

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Upgrade is called under selected-package admission and the session runtime
// exclusive lock. Browser input supplies neither members nor target settings.
type UpgradeRequest struct {
	Start              StartRequest
	ExpectedGeneration string
	Members            []agentbackend.ConversationRef
}
type UpgradeResult struct {
	Runtime, Previous RuntimeRef
	AppStorePath      string
	Phase             string
}
type UpgradeHooks struct {
	EnsureAttachment func(context.Context, RuntimeRef, HostCallbackConnector) error
	Publish          func(context.Context, UpgradeProof) error
}
type UpgradeTransition struct {
	Previous, Current RuntimeRef
	BindingRevision   uint64
	Members           []agentbackend.ConversationRef
}

// Only this package can construct the completed in-process publication proof.
// It is never serialized as another authority record or reusable permit.
type UpgradeProof struct{ transition *UpgradeTransition }

func (p UpgradeProof) Transition() (UpgradeTransition, error) {
	if p.transition == nil {
		return UpgradeTransition{}, ErrScope
	}
	result := *p.transition
	result.Members = append([]agentbackend.ConversationRef(nil), result.Members...)
	return result, nil
}

type hostStopper interface {
	Stop(context.Context, RuntimeRef, HostObservation) (HostObservation, error)
}

func (h nativeHost) Stop(ctx context.Context, ref RuntimeRef, observed HostObservation) (HostObservation, error) {
	if observed.InstanceID != ref.InstanceID || observed.RequestID != ref.RequestID || observed.Digest != ref.Digest || observed.Generation != ref.Generation || !uuidPattern.MatchString(observed.BootID) || len(observed.InvocationID) != 32 || !operationPattern.MatchString(observed.InvocationID) {
		return HostObservation{}, ErrScope
	}
	command := exec.CommandContext(ctx, "/run/wrappers/bin/sudo", "-n", h.path, "stop", "--instance", ref.InstanceID, "--request", ref.RequestID, "--digest", ref.Digest, "--boot-id", observed.BootID, "--invocation-id", observed.InvocationID)
	output, err := command.Output()
	return nativeHostOutput("stop", ref, ctx.Err(), output, err, nil)
}

type upgradeMember struct {
	record conversationRecord
	digest string
}

func sameUpgradeMember(a, b conversationRecord) bool {
	return a.Conversation == b.Conversation && a.CreationID == b.CreationID && a.Instructions == b.Instructions && a.HistoryRef == b.HistoryRef && a.HistoryFile == b.HistoryFile && a.Persisted == b.Persisted && a.Settings == b.Settings && a.InitialSettings == b.InitialSettings && reflect.DeepEqual(a.ToolPolicy, b.ToolPolicy)
}

func (m *Manager) upgradeMembers(ctx context.Context, request UpgradeRequest, records runtimeRecords) (map[string]agentbackend.ConversationRef, []upgradeMember, error) {
	directory, err := stateDirectory(m.config.UserStateRoot, request.Start.Session)
	if err != nil || len(request.Members) == 0 || len(request.Members) > 64 {
		return nil, nil, ErrScope
	}
	bound := make(map[string]agentbackend.ConversationRef)
	members := make([]upgradeMember, 0, len(request.Members))
	for _, ref := range request.Members {
		if validateHandle(agentbackend.ExecutionHandle{Conversation: ref, Generation: request.ExpectedGeneration}) != nil || ref.Session != request.Start.Session {
			return nil, nil, ErrScope
		}
		root := filepath.Join(directory, "members", ref.Member, ref.Incarnation)
		if _, duplicate := bound[root]; duplicate {
			return nil, nil, ErrScope
		}
		var record conversationRecord
		if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil {
			return nil, nil, err
		}
		if validateConversationRecord(record, ref, root) != nil || !bindingAllowed(m.config.AccountBindings, record.Settings) || !record.Persisted || record.HistoryFile == "" || (record.Generation != request.ExpectedGeneration && record.Generation != records.Requests[records.CurrentRequestID].Runtime.Generation) {
			return nil, nil, ErrScope
		}
		bound[root] = ref
		members = append(members, upgradeMember{record: record})
	}
	if err := m.assertRuntimeSettledExcept(ctx, records, bound); err != nil {
		return nil, nil, err
	}
	return bound, members, nil
}

// This is not public historical dispatch. A preparing direct successor is the
// only owner allowed to inspect/drain its exact still-running predecessor.
func (m *Manager) upgradePredecessorStatus(ctx context.Context, current, previous RuntimeRef) (RuntimeStatus, error) {
	records, err := m.records(current.Session)
	if err != nil {
		return RuntimeStatus{}, err
	}
	next, old := records.Requests[current.RequestID], records.Requests[previous.RequestID]
	if records.CurrentRequestID != current.RequestID || next.Runtime != current || next.Phase != "preparing" || next.Start.ReplacesRequestID != previous.RequestID || old.Runtime != previous || current.InstanceID != previous.InstanceID {
		return RuntimeStatus{}, ErrScope
	}
	observed, err := m.config.Host.Inspect(ctx, previous)
	if err != nil {
		return RuntimeStatus{}, err
	}
	if validTerminalReceipt(old, observed) {
		if err := m.config.verifyDrain(observed); err != nil {
			return RuntimeStatus{}, err
		}
		return RuntimeStatus{Runtime: previous, Phase: "stopped", Observation: observed}, nil
	}
	return m.inspectObservation(ctx, old, observed)
}

type upgradePredecessorConnector struct {
	manager           *Manager
	current, previous RuntimeRef
	lifetime          context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	revoked           bool
	streams           sync.WaitGroup
}

func (c *upgradePredecessorConnector) AttachHost(ctx context.Context, ref RuntimeRef, handler HostCallbackHandler) error {
	c.mu.Lock()
	if c.revoked || ref != c.previous {
		c.mu.Unlock()
		return ErrScope
	}
	c.streams.Add(1)
	c.mu.Unlock()
	defer c.streams.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	return c.manager.attachHostVerified(ctx, ref, handler, func() error {
		status, err := c.manager.upgradePredecessorStatus(ctx, c.current, c.previous)
		if err != nil {
			return err
		}
		if status.Phase != "ready" {
			return ErrHeld
		}
		return nil
	})
}
func (c *upgradePredecessorConnector) close() {
	c.mu.Lock()
	c.revoked = true
	c.cancel()
	c.mu.Unlock()
	c.streams.Wait()
}

func (m *Manager) Upgrade(ctx context.Context, request UpgradeRequest, hooks UpgradeHooks) (result UpgradeResult, resultErr error) {
	start := request.Start
	if hooks.EnsureAttachment == nil || hooks.Publish == nil || !uuidPattern.MatchString(start.RequestID) || !uuidPattern.MatchString(start.ReplacesRequestID) || start.RequestID == start.ReplacesRequestID || !hexPattern.MatchString(request.ExpectedGeneration) || start.BindingRevision == 0 || !storePattern.MatchString(start.AppStorePath) || !filepath.IsAbs(start.ProfilePath) || start.ProfileGeneration == "" {
		return result, ErrScope
	}
	directory, err := stateDirectory(m.config.UserStateRoot, start.Session)
	if err != nil {
		return result, err
	}
	unlock, err := runtimeLock(directory)
	if err != nil {
		return result, err
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	records, err := m.records(start.Session)
	if err != nil {
		return result, err
	}
	previous, ok := records.Requests[start.ReplacesRequestID]
	if !ok || previous.Runtime.Generation != request.ExpectedGeneration || previous.Runtime.Session != start.Session || previous.Phase != "starting" {
		return result, ErrScope
	}
	current, reserved := records.Requests[start.RequestID]
	if reserved {
		if records.CurrentRequestID != start.RequestID || current.Start != start {
			return result, ErrConflict
		}
	} else if records.CurrentRequestID != previous.Runtime.RequestID {
		return result, ErrConflict
	}
	if err := m.config.validatePackage(start.AppStorePath); err != nil {
		return result, err
	}
	if _, ok := m.config.Host.(hostStopper); !ok {
		return result, ErrUnavailable
	}
	_, members, err := m.upgradeMembers(ctx, request, records)
	if err != nil {
		return result, err
	}
	result.Previous, result.AppStorePath = previous.Runtime, start.AppStorePath
	result.Phase = "preparing"
	if !reserved {
		// Establish the real old attachment before fencing ordinary old handles.
		if err := hooks.EnsureAttachment(ctx, previous.Runtime, m); err != nil {
			return result, err
		}
		status, err := m.Inspect(ctx, previous.Runtime)
		if err != nil || status.Phase != "ready" {
			return result, ErrHeld
		}
		for i := range members {
			member := members[i].record
			// Loaded=false is the existing durable positive Unload completion.
			// A missing worker with Loaded=true still fails the live snapshot.
			if !member.Loaded {
				continue
			}
			var snapshot workerSnapshot
			handle := agentbackend.ExecutionHandle{Conversation: member.Conversation, Generation: previous.Runtime.Generation}
			if err := m.callRaw(ctx, previous.Runtime, "snapshot", conversationRequest{Handle: handle}, &snapshot); err != nil || snapshot.Busy || snapshot.Held || snapshot.NativeID != member.Conversation.NativeID || snapshot.HistoryFile != member.HistoryFile || snapshot.Persisted != member.Persisted || snapshot.Model != member.Settings.Model || snapshot.Provider != member.Settings.Provider || snapshot.Thinking != member.Settings.Thinking || snapshot.InstructionsDigest != instructionsDigest(member.Instructions) {
				return result, ErrHeld
			}
			if member.ToolPolicy != nil {
				if proof, err := m.InspectToolPolicy(ctx, handle); err != nil || ValidateToolPolicyProof(proof, *member.ToolPolicy, previous.Runtime.Generation) != nil {
					return result, ErrHeld
				}
			}
			members[i].digest = snapshot.HistoryDigest
		}
		descriptor, err := readPrivateBytes(filepath.Join(m.instance(previous.Runtime), "launch.json"), 65536)
		if err != nil {
			return result, err
		}
		current, err = m.reserveSuccessorRecord(start, records, previous, descriptor)
		if err != nil {
			return result, err
		}
	}
	result.Runtime = current.Runtime
	if current.Phase == "preparing" {
		// Retention failures keep the intent and do not begin predecessor drain.
		if err := m.retainRuntimeRequest(ctx, previous); err != nil {
			return result, err
		}
		if err := m.retainRuntimeRequest(ctx, current); err != nil {
			return result, err
		}
		status, err := m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime)
		if err != nil {
			return result, err
		}
		if status.Phase != "stopped" {
			if status.Phase != "ready" {
				return result, ErrHeld
			}
			lifetime, cancel := context.WithCancel(ctx)
			connector := &upgradePredecessorConnector{manager: m, current: current.Runtime, previous: previous.Runtime, lifetime: lifetime, cancel: cancel}
			defer connector.close()
			if err := hooks.EnsureAttachment(ctx, previous.Runtime, connector); err != nil {
				return result, err
			}
			for _, member := range members {
				if _, err := m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime); err != nil {
					return result, err
				}
				handle := agentbackend.ExecutionHandle{Conversation: member.record.Conversation, Generation: previous.Runtime.Generation}
				var stopped StopResult
				if err := m.callRaw(ctx, previous.Runtime, "unload", conversationRequest{Handle: handle}, &stopped); err != nil || stopped.Outcome != "stopped" || !stopped.VerifiedExit {
					return result, ErrHeld
				}
			}
			status, err = m.upgradePredecessorStatus(ctx, current.Runtime, previous.Runtime)
			if err != nil || status.Phase != "ready" {
				return result, ErrHeld
			}
			stopper, ok := m.config.Host.(hostStopper)
			if !ok {
				return result, ErrUnavailable
			}
			observed, err := stopper.Stop(ctx, previous.Runtime, status.Observation)
			if err != nil {
				return result, err
			}
			if !validTerminalReceipt(previous, observed) || m.config.verifyDrain(observed) != nil {
				return result, ErrHeld
			}
		}
		fresh, err := m.records(start.Session)
		if err != nil {
			return result, err
		}
		if err := m.assertRuntimeSettled(ctx, fresh); err != nil {
			return result, err
		}
	}
	// Ensure owns the existing runtime.lock and preserves inspect-only replay
	// after starting. The outer session EX lock still fences other operations.
	unlock()
	unlock = nil
	status, err := m.Ensure(ctx, start)
	result.Phase = status.Phase
	if err != nil {
		return result, err
	}
	if status.Runtime != current.Runtime || status.Phase != "ready" {
		return result, ErrHeld
	}
	if err := hooks.EnsureAttachment(ctx, current.Runtime, m); err != nil {
		return result, err
	}
	unlock, err = runtimeLock(directory)
	if err != nil {
		return result, err
	}
	fresh, err := m.records(start.Session)
	if err != nil || fresh.Requests[fresh.CurrentRequestID].Runtime != current.Runtime {
		return result, ErrScope
	}
	for _, member := range members {
		handle := agentbackend.ExecutionHandle{Conversation: member.record.Conversation, Generation: current.Runtime.Generation}
		loaded, err := m.Load(ctx, handle)
		if err != nil || loaded.Conversation != handle.Conversation || loaded.Generation != handle.Generation || loaded.State != "idle" || loaded.HistoryRef != member.record.HistoryRef || loaded.Settings != member.record.Settings {
			return result, ErrHeld
		}
		root := filepath.Join(directory, "members", handle.Conversation.Member, handle.Conversation.Incarnation)
		var record conversationRecord
		if err := readPrivate(filepath.Join(root, "conversation.json"), &record); err != nil || !sameUpgradeMember(member.record, record) || !record.Loaded || record.Generation != handle.Generation {
			return result, ErrScope
		}
		var snapshot workerSnapshot
		if err := m.handleCall(ctx, handle, "snapshot", conversationRequest{}, &snapshot); err != nil || snapshot.Busy || snapshot.Held || (member.digest != "" && snapshot.HistoryDigest != member.digest) {
			return result, ErrHeld
		}
	}
	transition := UpgradeTransition{Previous: previous.Runtime, Current: current.Runtime, BindingRevision: start.BindingRevision, Members: append([]agentbackend.ConversationRef(nil), request.Members...)}
	if err := hooks.Publish(ctx, UpgradeProof{transition: &transition}); err != nil {
		return result, err
	}
	result.Phase = "ready"
	return result, nil
}
