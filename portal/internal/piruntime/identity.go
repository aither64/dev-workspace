package piruntime

import (
	"crypto/rand"
	"encoding/hex"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"path/filepath"
)

// historyReference is a locator for the owned mapping, never persistence proof.
// The ordered versioned JSON encoding is part of the on-disk contract.
func historyReference(ref agentbackend.ConversationRef) string {
	return digestValue([]any{"pi-history", 1, ref.Backend, ref.Session.Workspace,
		ref.Session.Slug, ref.Session.Incarnation, ref.Member, ref.Incarnation, ref.NativeID})
}

func randomHex(size int) string {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data)
}
func newUUID() string {
	raw := randomHex(16)
	return raw[:8] + "-" + raw[8:12] + "-4" + raw[13:16] + "-a" + raw[17:20] + "-" + raw[20:]
}

func validateHandle(handle agentbackend.ExecutionHandle) error {
	r := handle.Conversation
	if err := validateSession(r.Session); err != nil {
		return err
	}
	if r.Backend != agentbackend.Pi || !memberPattern.MatchString(r.Member) || !hexPattern.MatchString(r.Incarnation) || !uuidPattern.MatchString(r.NativeID) || !hexPattern.MatchString(handle.Generation) {
		return ErrScope
	}
	return nil
}

// BackendIdentity projects trusted adapter routing; it grants no authorization.
// An invalid handle has no projection and is rejected by every bound operation.
func BackendIdentity(handle agentbackend.ExecutionHandle) backend.Identity {
	if validateHandle(handle) != nil {
		return backend.Identity{}
	}
	return backend.Identity{BackendID: "pi", ScopeKey: historyReference(handle.Conversation), ConversationID: handle.Conversation.NativeID, Directory: filepath.Join(handle.Conversation.Session.Workspace, "work", handle.Conversation.Session.Slug), Generation: handle.Generation}
}
func validateSession(ref agentbackend.SessionRef) error {
	if !filepath.IsAbs(ref.Workspace) || filepath.Clean(ref.Workspace) != ref.Workspace || !slugPattern.MatchString(ref.Slug) || !hexPattern.MatchString(ref.Incarnation) {
		return ErrScope
	}
	return nil
}

func sessionDirectory(ref agentbackend.SessionRef) (string, error) {
	if err := validateSession(ref); err != nil {
		return "", err
	}
	path := filepath.Join(ref.Workspace, "work", ref.Slug)
	canonical, err := ownedDirectory(path)
	if err != nil || canonical != path {
		return "", ErrScope
	}
	return path, nil
}
