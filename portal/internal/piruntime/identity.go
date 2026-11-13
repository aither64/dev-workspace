package piruntime

import (
	"crypto/rand"
	"encoding/hex"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
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
