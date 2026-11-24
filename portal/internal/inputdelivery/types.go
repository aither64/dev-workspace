// Package inputdelivery owns the portal inbox, independently of native receipts.
package inputdelivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// ConversationScope is the existing stable browser namespace, excluding runtime
// generation and native history location. Refusal identities bind it to a real
// locally authorized conversation, without a native submission reference.
func ConversationScope(ref agentbackend.ConversationRef) string {
	return "pi-workflow-v1:" + Digest([]string{"pi-workflow-v1", string(ref.Backend), ref.Session.Workspace, ref.Session.Slug, ref.Session.Incarnation, ref.Member, ref.Incarnation})
}

func SameRequest(a, b backend.DeliveryRequest) bool {
	return a.Version == b.Version && a.ClientUserMessageID == b.ClientUserMessageID && a.Message == b.Message &&
		a.Selection == b.Selection && a.Delivery == b.Delivery && slices.Equal(a.AttachmentIDs, b.AttachmentIDs)
}

func validRequest(r backend.DeliveryRequest) bool {
	if r.Version != 1 || !idPattern.MatchString(r.ClientUserMessageID) || !digestPattern.MatchString(r.Selection) ||
		strings.TrimSpace(r.Message) != r.Message || len(r.Message) > 20000 || r.Message == "" && len(r.AttachmentIDs) == 0 || len(r.AttachmentIDs) > 100 ||
		(r.Delivery != "" && r.Delivery != "steer" && r.Delivery != "followUp") {
		return false
	}
	seen := map[string]bool{}
	for _, id := range r.AttachmentIDs {
		if !idPattern.MatchString(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

type Refusal struct {
	Version      int
	Scope        string
	Conversation agentbackend.ConversationRef
	Request      backend.DeliveryRequest
	Code         string
	CreatedAtMS  int64
}

func (r Refusal) Validate() error {
	if r.Version != 1 || r.CreatedAtMS <= 0 || !validRequest(r.Request) || piquestions.ValidateRef(r.Conversation) != nil || r.Conversation.Backend != agentbackend.Pi || r.Scope != ConversationScope(r.Conversation) {
		return ErrInvalid
	}
	switch r.Code {
	case "delivery_selection_changed", "delivery_invalid", "delivery_capacity", "delivery_unavailable":
		return nil
	}
	return ErrInvalid
}
func (r Refusal) disposition() error {
	return &backend.DeliveryError{Code: r.Code, NotQueued: &backend.DeliveryNotQueuedProof{Version: 1, Scope: r.Scope, Request: r.Request}}
}

var (
	ErrConflict   = errors.New("delivery identity changed")
	ErrCapacity   = errors.New("delivery capacity reached")
	ErrInvalid    = errors.New("invalid delivery state")
	ErrNotFound   = errors.New("delivery not found")
	idPattern     = regexp.MustCompile(`^([0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Selection is the immutable local observation accepted by the portal. It grants
// no execution authority; the dispatch owner must revalidate the entire tuple.
type Selection struct {
	Runtime            piruntime.RuntimeRef
	Handle             agentbackend.ExecutionHandle
	Settings           piruntime.EffectiveSettings
	Policy             *piruntime.ToolPolicyTarget `json:",omitempty"`
	InstructionsDigest string
	Access             string
	BindingRevision    uint64
}

func Digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type Admission struct {
	Version           int
	Scope             string
	Request           backend.DeliveryRequest
	Wire, UploadScope string
	Selection         Selection
	Ref               piruntime.SubmissionRef
	Input             piruntime.SendInput
}

func (a Admission) Validate() error {
	r, s := a.Request, a.Selection
	if a.Version != 1 || a.Scope != ConversationScope(a.Ref.Conversation) || !validRequest(r) ||
		r.Selection != Digest(s) || a.Ref.Conversation != s.Handle.Conversation || a.Ref.OperationID != r.ClientUserMessageID || a.Ref.Action != "send" ||
		a.Ref.Digest != piruntime.SubmissionDigest(a.Ref, a.Input, s.Settings) || a.Input.Message != a.Wire || a.Input.Retry ||
		a.Wire == "" || len(a.Wire) > 20000 || len(r.Message) > 20000 || len(r.AttachmentIDs) > 100 ||
		s.Handle.Conversation.Backend != agentbackend.Pi || piquestions.ValidateRef(s.Handle.Conversation) != nil || s.Runtime.Session != s.Handle.Conversation.Session || s.Runtime.Generation != s.Handle.Generation ||
		!digestPattern.MatchString(s.Handle.Generation) || !digestPattern.MatchString(s.InstructionsDigest) || s.Settings.Model == "" || s.Settings.Revision == 0 ||
		s.BindingRevision == 0 || (s.Access != "workspace_write" && s.Access != "read_only") || s.Policy != nil && piruntime.ValidateToolPolicy(*s.Policy) != nil {
		return ErrInvalid
	}
	if r.Delivery != "" && r.Delivery != "steer" && r.Delivery != "followUp" {
		return ErrInvalid
	}
	context := ""
	if r.Delivery != "" {
		context = "pi-delivery-v1:" + r.Delivery
	}
	if a.Input.ActionContext != context {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range r.AttachmentIDs {
		if !idPattern.MatchString(id) || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	return nil
}

type Record struct {
	Admission          Admission
	Status             backend.DeliveryStatus
	NextAttemptMS      int64
	DispatchGeneration string
	DispatchAtMS       int64
	RetryDelayMS       int64
	DispatchRuntime    piruntime.RuntimeRef
	DispatchHandle     agentbackend.ExecutionHandle
}

type ExecutionEvidence struct {
	Runtime piruntime.RuntimeRef
	Handle  agentbackend.ExecutionHandle
}

func (r Record) Validate() error {
	a, v := r.Admission, r.Status
	if a.Validate() != nil || v.Version != 1 || v.Scope != a.Scope || v.ClientUserMessageID != a.Request.ClientUserMessageID || v.Selection != a.Request.Selection ||
		v.Message != a.Request.Message || Digest(v.AttachmentIDs) != Digest(a.Request.AttachmentIDs) || v.Delivery != a.Request.Delivery || v.Sequence == 0 ||
		v.CreatedAtMS <= 0 || v.UpdatedAtMS < v.CreatedAtMS || len(v.Failure) > 512 || v.Retired && v.State != "unknown" {
		return ErrInvalid
	}
	if v.State == "dispatching" || v.State == "pending" || v.State == "unknown" || v.State == "sent" {
		if r.DispatchAtMS <= 0 || r.DispatchHandle.Conversation != a.Ref.Conversation || r.DispatchRuntime.Session != a.Ref.Conversation.Session ||
			r.DispatchRuntime.Generation != r.DispatchHandle.Generation || r.DispatchGeneration != r.DispatchHandle.Generation || !digestPattern.MatchString(r.DispatchGeneration) {
			return ErrInvalid
		}
	}
	if v.State == "sent" {
		if v.Receipt == nil || v.Receipt.ClientUserMessageID != v.ClientUserMessageID || v.Receipt.TurnID == "" {
			return ErrInvalid
		}
		return nil
	}
	if v.Receipt != nil {
		return ErrInvalid
	}
	switch v.State {
	case "queued", "dispatching", "pending", "not_sent", "unknown":
		return nil
	}
	return ErrInvalid
}
