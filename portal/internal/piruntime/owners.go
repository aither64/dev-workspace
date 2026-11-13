package piruntime

import "github.com/aither64/dev-workspace/portal/internal/agentbackend"

// Retained fields keep their original wire order and bytes. Utilities have an
// explicit, disjoint arm, never an invented retained member or ConversationRef.
type workerIdentity struct {
	Conversation agentbackend.ConversationRef `json:",omitzero"`
	Generation   string
	OwnerKind    string      `json:",omitempty"`
	Utility      *UtilityRef `json:",omitempty"`
}

func retainedWorkerIdentity(handle agentbackend.ExecutionHandle) workerIdentity {
	return workerIdentity{Conversation: handle.Conversation, Generation: handle.Generation}
}
func (i workerIdentity) nativeID() string {
	if i.OwnerKind == "utility" && i.Utility != nil {
		return i.Utility.NativeID
	}
	return i.Conversation.NativeID
}
func (i workerIdentity) subject() CallbackSubject {
	if i.OwnerKind == "utility" && i.Utility != nil {
		u := i.Utility
		return CallbackSubject{Kind: "utility", Utility: &UtilityCallbackSubject{Session: u.Session, MonitorOperationID: u.OperationID, Incarnation: u.Incarnation, NativeID: u.NativeID, Generation: i.Generation}}
	}
	h := agentbackend.ExecutionHandle{Conversation: i.Conversation, Generation: i.Generation}
	return CallbackSubject{Kind: "conversation", Conversation: &h}
}
func (c *conversation) identity(generation string) workerIdentity {
	if c.record.OwnerKind == "utility" && c.record.Utility != nil {
		u := *c.record.Utility
		return workerIdentity{OwnerKind: "utility", Utility: &u, Generation: generation}
	}
	return workerIdentity{Conversation: c.record.Conversation, Generation: generation}
}
func (c *conversation) matchesReceipt(ref receiptReference) bool {
	if c.record.OwnerKind == "utility" {
		return c.record.Utility != nil && ref.kind() == "utility" && ref.utility.Utility == *c.record.Utility
	}
	return ref.kind() == "retained" && ref.retained.Conversation == c.record.Conversation
}

type ownedConversationState struct {
	Generation, State string
	Persisted         bool
	Settings          EffectiveSettings
}

func retainedState(c *conversation, state ownedConversationState) ConversationState {
	return ConversationState{Conversation: c.record.Conversation, Generation: state.Generation, State: state.State, HistoryRef: c.record.HistoryRef, Persisted: state.Persisted, Settings: state.Settings}
}
func utilityState(c *conversation, state ownedConversationState) UtilityState {
	if !state.Persisted && state.State == "idle" {
		state.State = "initialized"
	}
	return UtilityState{Utility: *c.record.Utility, Generation: state.Generation, State: state.State, Persisted: state.Persisted, Settings: state.Settings}
}
