package web

// Keep this structural interface optional for old shared clients. Wrapping a
// client must not advertise typed invalidations when its source lacks them.
type pendingRevisionProvider interface {
	PendingRevision(string) uint64
}

type pendingRevisionForwarder struct{ pendingRevisionProvider }

type memberPendingConversationClient struct {
	memberConversationClient
	pendingRevisionForwarder
}

type memberPagedPendingConversationClient struct {
	memberPagedConversationClient
	pendingRevisionForwarder
}

type recoveryPendingConversationClient struct {
	recoveryConversationClient
	pendingRevisionForwarder
}

type recoveryPagedPendingConversationClient struct {
	recoveryPagedConversationClient
	pendingRevisionForwarder
}
