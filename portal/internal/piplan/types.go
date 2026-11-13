// Package piplan retains exact displayed Plan proposals and acceptance intent.
// It advertises no mode enforcement or native implementation capability.
package piplan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

type Caller = piquestions.Caller
type NativeOrigin struct {
	EntryID  string `json:"entryId"`
	Complete bool   `json:"complete"`
}
type Proposal struct {
	PlanID       string                       `json:"planId"`
	Revision     uint64                       `json:"revision"`
	Conversation agentbackend.ConversationRef `json:"conversation"`
	Caller       Caller                       `json:"caller"`
	NativeOrigin NativeOrigin                 `json:"nativeOrigin"`
	Text         string                       `json:"text"`
	TextSHA256   string                       `json:"textSha256"`
	Complete     bool                         `json:"complete"`
	CreatedAt    time.Time                    `json:"createdAt"`
}
type AcceptanceInput struct {
	AcceptanceID string `json:"acceptanceId"`
	PlanID       string `json:"planId"`
	Revision     uint64 `json:"revision"`
	TextSHA256   string `json:"textSha256"`
}
type Acceptance struct {
	AcceptanceInput
	OriginalMode   string `json:"originalMode"`
	TargetMode     string `json:"targetMode"`
	PolicyRevision string `json:"policyRevision"`
	OperationID    string `json:"operationId"`
	Phase          string `json:"phase"`
	ReceiptRef     string `json:"receiptRef"`
	ActionContext  string `json:"actionContext"`
	Message        string `json:"message"`
}
type Record struct {
	Schema       int                          `json:"schema"`
	Conversation agentbackend.ConversationRef `json:"conversation"`
	Revision     uint64                       `json:"revision"`
	Proposals    []Proposal                   `json:"proposals"`
	Acceptances  []Acceptance                 `json:"acceptances"`
}
type ProposalView struct {
	PlanID     string `json:"planId"`
	Revision   uint64 `json:"revision"`
	Text       string `json:"text"`
	TextSHA256 string `json:"textSha256"`
	Complete   bool   `json:"complete"`
	CanAccept  bool   `json:"canAccept"`
}
type AcceptanceView struct {
	AcceptanceInput
	OperationID string `json:"operationId"`
	Phase       string `json:"phase"`
}
type Snapshot struct {
	Revision           uint64           `json:"revision"`
	Proposals          []ProposalView   `json:"proposals"`
	Acceptances        []AcceptanceView `json:"acceptances"`
	CanSelectMode      bool             `json:"canSelectMode"`
	Mode               string           `json:"mode,omitempty"`
	ToolPolicyRevision uint64           `json:"toolPolicyRevision,omitempty"`
	ToolPolicyDigest   string           `json:"toolPolicyDigest,omitempty"`
}
type ModeIntent struct {
	Mode           string `json:"mode"`
	PolicyRevision string `json:"policyRevision"`
}

func (m ModeIntent) Validate() error {
	if (m.Mode != "plan" && m.Mode != "default") || !piquestions.Text(m.PolicyRevision, 4096) {
		return piquestions.ErrInvalid
	}
	return nil
}
func Digest(text string) string { h := sha256.Sum256([]byte(text)); return hex.EncodeToString(h[:]) }

// Policy is positive enforcement proof supplied under host admission. A saved
// mode or policy revision alone cannot create this proof.
type Policy struct {
	OriginalMode, Revision   string
	Idle, Eligible, Enforced bool
}
type PolicyCheck func(context.Context, agentbackend.ConversationRef, Caller) (Policy, error)
