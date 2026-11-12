// Package agentbackend defines host-owned identities shared by native adapters.
// It contains no transport, storage, authorization or execution implementation.
package agentbackend

import "errors"

type Kind string

const (
	Codex Kind = "codex"
	Pi    Kind = "pi"
)

var (
	ErrNotBound             = errors.New("session backend is not bound")
	ErrUnsupportedBinding   = errors.New("unsupported session backend binding")
	ErrBindingConflict      = errors.New("session backend binding conflict")
	ErrConversationNotReady = errors.New("backend conversation is not ready")
	ErrStaleExecution       = errors.New("stale backend execution handle")
)

type SessionRef struct {
	Workspace, Slug, Incarnation string
}

// ConversationRef is stable across worker restarts. NativeID is opaque; only
// the native adapter can prove its association with owned history. For legacy
// Codex, incarnations come from existing validated evidence, never a new UUID.
type ConversationRef struct {
	Session                       SessionRef
	Backend                       Kind
	Member, Incarnation, NativeID string
}

// ExecutionHandle adds current dispatch evidence to a stable conversation.
// Generation must never become a new receipt/idempotency namespace. Possessing
// either identity does not grant roster access, native ownership or permission
// to execute, and does not prove that a backend accepted a submission.
type ExecutionHandle struct {
	Conversation ConversationRef
	Generation   string
}
