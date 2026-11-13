// Package piquestions stores host-owned optional questions. It never selects or
// starts a native worker. Callers must supply trusted scope and authorization.
package piquestions

import (
	"context"
	"errors"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

var (
	ErrInvalid     = errors.New("invalid Pi question input")
	ErrConflict    = errors.New("Pi question identity conflict")
	ErrNotFound    = errors.New("Pi question not found")
	ErrUnavailable = errors.New("Pi question operation unavailable")
	ErrCapacity    = errors.New("Pi question store capacity exceeded")
)

const MessageLimit = 20000
const StoreLimit = 4 << 20

type LineageRef struct {
	AnchorEntryID  string `json:"anchorEntryId"`
	SelectionToken string `json:"selectionToken"`
}
type Caller struct {
	Handle           agentbackend.ExecutionHandle `json:"handle"`
	Lineage          LineageRef                   `json:"lineage"`
	NativeToolCallID string                       `json:"nativeToolCallId"`
}
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}
type Question struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Options []Option `json:"options"`
}
type PublishInput struct {
	Questions []Question `json:"questions"`
}
type Answer struct {
	QuestionID string `json:"questionId"`
	ChoiceID   string `json:"choiceId"`
	Text       string `json:"text"`
}
type ResponseInput struct {
	ResponseID           string   `json:"responseId"`
	QuestionID           string   `json:"questionId"`
	QuestionRevision     uint64   `json:"questionRevision"`
	SupersedesResponseID string   `json:"supersedesResponseId"`
	Answers              []Answer `json:"answers"`
}
type Delivery struct {
	Phase         string `json:"phase"`
	Message       string `json:"message"`
	OperationID   string `json:"operationId"`
	ActionContext string `json:"actionContext"`
	ReceiptRef    string `json:"receiptRef"`
}
type ResponseRecord struct {
	Input      ResponseInput `json:"input"`
	Sequence   uint64        `json:"sequence"`
	AcceptedAt time.Time     `json:"acceptedAt"`
	Delivery   Delivery      `json:"delivery"`
}
type QuestionRecord struct {
	ID        string           `json:"id"`
	Revision  uint64           `json:"revision"`
	Caller    Caller           `json:"caller"`
	Questions []Question       `json:"questions"`
	CreatedAt time.Time        `json:"createdAt"`
	Closed    bool             `json:"closed"`
	Responses []ResponseRecord `json:"responses"`
}
type Record struct {
	Schema       int                          `json:"schema"`
	Conversation agentbackend.ConversationRef `json:"conversation"`
	Revision     uint64                       `json:"revision"`
	NextSequence uint64                       `json:"nextSequence"`
	Questions    []QuestionRecord             `json:"questions"`
}

// Public views contain no native identities, paths, lineage tokens or handles.
type ResponseView struct {
	ResponseInput
	Sequence      uint64 `json:"sequence"`
	Status        string `json:"status"`
	DeliveryPhase string `json:"deliveryPhase"`
}
type QuestionView struct {
	ID             string            `json:"id"`
	Revision       uint64            `json:"revision"`
	Recipient      string            `json:"recipient"`
	Questions      []Question        `json:"questions"`
	Answers        map[string]Answer `json:"answers"`
	Status         string            `json:"status"`
	LatestResponse *ResponseView     `json:"latestResponse,omitempty"`
	CanRespond     bool              `json:"canRespond"`
}
type Snapshot struct {
	Revision  uint64         `json:"revision"`
	Questions []QuestionView `json:"questions"`
}
type Store interface {
	Publish(context.Context, Caller, PublishInput) (QuestionView, error)
	Snapshot(context.Context, agentbackend.ConversationRef) (Snapshot, error)
	AcceptResponse(context.Context, agentbackend.ConversationRef, ResponseInput) (ResponseView, error)
	Close(context.Context, Caller, string, uint64) (QuestionView, error)
}

// Validate is observational. It must resolve current recipient, branch and
// authority from host evidence under the owner's admission locks; it must not
// probe or activate a native worker. Generation is not durable identity.
type Validate func(context.Context, agentbackend.ConversationRef, *Caller) error
