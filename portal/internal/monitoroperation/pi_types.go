package monitoroperation

import "github.com/aither64/dev-workspace/portal/internal/agentbackend"

// PiUtilityHandle is an operation-owned projection. It is deliberately not a
// retained-member ConversationRef and cannot enter P3's Create/Load/Bind APIs.
type PiUtilityHandle struct {
	Session                                        agentbackend.SessionRef
	OperationID, Incarnation, NativeID, Generation string
}

type PiUtilityPolicy struct {
	PackageRoot, CatalogDigest, Model, Effort, Instructions string
	SavedAccess, ExecutionAccess                            string
	MaxConcurrent                                           int
}

// PiUtilityLaunch references evidence owned by the existing monitor operation.
// It adds no writer/codec, launch receipt ledger or authority to execute.
type PiUtilityLaunch struct {
	Handle                                           PiUtilityHandle
	LaunchOperationID, InputReceiptID, CreationProof string
	Policy                                           PiUtilityPolicy
	Request                                          StartRequest
}

type PiUtilitySettlement struct {
	SourceComplete, EffectsSettled, ReportPublished bool
}
