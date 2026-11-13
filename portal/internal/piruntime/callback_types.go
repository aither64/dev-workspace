package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

type HostCallbackConnector interface {
	AttachHost(context.Context, RuntimeRef, HostCallbackHandler) error
}
type HostCallbackHandler interface {
	HandleHostCall(context.Context, HostCall) (HostReply, error)
}
type HostToolProvider interface{ HostTools() []string }
type HostCall struct {
	Runtime                                           RuntimeRef
	Subject                                           CallbackSubject
	InputOperationID, AnchorEntryID, NativeToolCallID string
	SelectionToken, Tool, ArgumentsSHA256             string
	Arguments                                         json.RawMessage
}
type CallbackSubject struct {
	Kind         string
	Conversation *agentbackend.ExecutionHandle
	Utility      *UtilityCallbackSubject
}
type UtilityCallbackSubject struct {
	Session                                               agentbackend.SessionRef
	MonitorOperationID, Incarnation, NativeID, Generation string
}
type HostReply struct {
	Outcome string
	Body    json.RawMessage
	Code    string
}

// Historical origin inspection reads the retained invocation. It grants no
// callback authority and does not infer an active input from the current turn.
type NativeOriginRef struct {
	AnchorEntryID, SelectionToken, NativeToolCallID, Tool string
}
type NativeOriginObservation struct {
	Handle    agentbackend.ExecutionHandle
	Origin    NativeOriginRef
	Arguments json.RawMessage
}
type NativeOriginInspector interface {
	InspectNativeOrigin(context.Context, agentbackend.ExecutionHandle, NativeOriginRef) (NativeOriginObservation, error)
}

func validateNativeOrigin(ref NativeOriginRef) error {
	for _, value := range []string{ref.AnchorEntryID, ref.NativeToolCallID, ref.Tool} {
		if value == "" || len(value) > 4096 || !validString(value) {
			return ErrScope
		}
	}
	const prefix = "linear-v1:"
	if len(ref.SelectionToken) != len(prefix)+64 || ref.SelectionToken[:len(prefix)] != prefix || !hexPattern.MatchString(ref.SelectionToken[len(prefix):]) {
		return ErrScope
	}
	return nil
}

func lineageBytes(tuple []string) ([]byte, error) {
	if len(tuple) > 64 {
		return nil, ErrScope
	}
	data := binary.BigEndian.AppendUint32(nil, uint32(len(tuple)))
	for _, value := range tuple {
		if !utf8.ValidString(value) || len(value) > 65536 {
			return nil, ErrScope
		}
		data = binary.BigEndian.AppendUint32(data, uint32(len(value)))
		data = append(data, []byte(value)...)
	}
	return data, nil
}
func LinearSelectionToken(subject CallbackSubject, root string) (string, error) {
	var tuple []string
	if root == "" {
		return "", ErrScope
	}
	switch subject.Kind {
	case "conversation":
		if subject.Conversation == nil || subject.Utility != nil {
			return "", ErrScope
		}
		handle := *subject.Conversation
		if err := validateHandle(handle); err != nil {
			return "", err
		}
		ref := handle.Conversation
		tuple = []string{"conversation", ref.Session.Workspace, ref.Session.Slug, ref.Session.Incarnation, ref.Member, ref.Incarnation, ref.NativeID, root}
	case "utility":
		if subject.Utility == nil || subject.Conversation != nil {
			return "", ErrScope
		}
		u := subject.Utility
		if validateSession(u.Session) != nil || !operationPattern.MatchString(u.MonitorOperationID) || !hexPattern.MatchString(u.Incarnation) || !uuidPattern.MatchString(u.NativeID) || !hexPattern.MatchString(u.Generation) {
			return "", ErrScope
		}
		tuple = []string{"utility", u.Session.Workspace, u.Session.Slug, u.Session.Incarnation, u.MonitorOperationID, u.Incarnation, u.NativeID, root}
	default:
		return "", ErrScope
	}
	bytes, err := lineageBytes(tuple)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes)
	return "linear-v1:" + hex.EncodeToString(sum[:]), nil
}
