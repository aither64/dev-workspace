package monitoroperation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type PiUtilityRecord struct {
	Session     PiSessionRecord `json:"session"`
	OperationID string          `json:"operationId"`
	Incarnation string          `json:"incarnation"`
	NativeID    string          `json:"nativeId,omitempty"`
}

func (u *PiUtilityRecord) UnmarshalJSON(data []byte) error {
	type value PiUtilityRecord
	var decoded value
	if err := DecodePiRecord(data, &decoded, 16*1024); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, present := fields["nativeId"]; present && decoded.NativeID == "" {
		return piruntime.ErrScope
	}
	*u = PiUtilityRecord(decoded)
	return u.Validate(false)
}

func (u PiUtilityRecord) Validate(requireNative bool) error {
	if u.Session.Validate() != nil || !validID(u.OperationID) || !piUtilityHex.MatchString(u.Incarnation) ||
		(u.NativeID != "" && !piRecordText(u.NativeID, 4096)) || (requireNative && u.NativeID == "") {
		return piruntime.ErrScope
	}
	return nil
}

type PiUtilityInputRecord struct {
	Utility     PiUtilityRecord `json:"utility"`
	OperationID string          `json:"operationId"`
	Action      string          `json:"action"`
	Digest      string          `json:"digest"`
}

func (r PiUtilityInputRecord) Validate() error {
	if r.Utility.Validate(true) != nil || !validID(r.OperationID) || !piRecordText(r.Action, 4096) || !piUtilityHex.MatchString(r.Digest) {
		return piruntime.ErrScope
	}
	return nil
}

type PiOwnerRecord struct {
	Session     PiSessionRecord      `json:"session"`
	Lead        PiConversationRecord `json:"lead"`
	OriginInput PiInputRecord        `json:"originInput"`
	Generation  string               `json:"generation"`
}
type PiUtilityPolicyRecord struct {
	Kind            string           `json:"kind"`
	PackageRoot     string           `json:"packageRoot"`
	CatalogDigest   string           `json:"catalogDigest"`
	Model           string           `json:"model"`
	Effort          string           `json:"effort"`
	Instructions    string           `json:"instructions"`
	SavedAccess     string           `json:"savedAccess"`
	ExecutionAccess string           `json:"executionAccess"`
	MaxConcurrent   int              `json:"maxConcurrent"`
	Settings        PiSettingsRecord `json:"settings"`
}
type PiLaunchRecord struct {
	State           string                `json:"state"`
	Utility         PiUtilityRecord       `json:"utility"`
	CreationID      string                `json:"creationId"`
	CreateAttempted bool                  `json:"createAttempted"`
	MessageID       string                `json:"messageId"`
	Envelope        string                `json:"envelope"`
	Settings        PiSettingsRecord      `json:"settings"`
	Input           *PiUtilityInputRecord `json:"input,omitempty"`
	Outcome         string                `json:"outcome,omitempty"`
	Generation      string                `json:"generation,omitempty"`
	TerminatedAt    *time.Time            `json:"terminatedAt,omitempty"`
	Termination     string                `json:"termination,omitempty"`
	Detail          string                `json:"detail,omitempty"`
}
type PiEventDelivery struct {
	Status     string            `json:"status"`
	MessageID  string            `json:"messageId"`
	Envelope   string            `json:"envelope"`
	Context    string            `json:"context"`
	Detail     string            `json:"detail,omitempty"`
	PiDelivery *PiDeliveryRecord `json:"piDelivery,omitempty"`
}
type PiEventRecord struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"createdAt"`
	Report    ReportRequest   `json:"report"`
	Delivery  PiEventDelivery `json:"delivery"`
}
type PiResolutionRecord struct {
	ID       string               `json:"id"`
	At       time.Time            `json:"at"`
	Lead     PiConversationRecord `json:"lead"`
	Evidence []string             `json:"evidence"`
	Detail   string               `json:"detail"`
}
type PiSettlementRecord struct {
	State              string    `json:"state"`
	SourceEvidence     []string  `json:"sourceEvidence"`
	ToolEvidence       []string  `json:"toolEvidence"`
	ReportEventID      string    `json:"reportEventId"`
	ObservedGeneration string    `json:"observedGeneration"`
	ObservedAt         time.Time `json:"observedAt"`
}
type PiOperation struct {
	Schema            int                   `json:"schema"`
	ID                string                `json:"id"`
	Workspace         string                `json:"workspace"`
	Source            Source                `json:"source"`
	Brief             string                `json:"brief"`
	CancellationScope string                `json:"cancellationScope,omitempty"`
	RequestDigest     string                `json:"requestDigest"`
	CreatedAt         time.Time             `json:"createdAt"`
	Events            []PiEventRecord       `json:"events"`
	CanceledAt        *time.Time            `json:"canceledAt,omitempty"`
	Disposition       string                `json:"disposition,omitempty"`
	Resolution        *PiResolutionRecord   `json:"resolution,omitempty"`
	Backend           string                `json:"backend"`
	BackendContract   int                   `json:"backendContract"`
	Owner             PiOwnerRecord         `json:"owner"`
	UtilityPolicy     PiUtilityPolicyRecord `json:"utilityPolicy"`
	Launch            PiLaunchRecord        `json:"launch"`
	Settlement        *PiSettlementRecord   `json:"settlement,omitempty"`
}

func (p PiOperation) Validate() error {
	req := StartRequest{ID: p.ID, Brief: p.Brief, Source: p.Source, CancellationScope: p.CancellationScope}
	o, u, l := p.Owner, p.UtilityPolicy, p.Launch
	if p.Disposition != "" && p.Disposition != "archiving" && p.Disposition != "archived" {
		return piruntime.ErrScope
	}
	if p.Schema != 2 || p.Backend != "pi" || p.BackendContract != 1 || o.Session.Validate() != nil || p.Workspace != o.Session.Workspace || o.Lead.Validate() != nil || o.Lead.Member != "lead" || o.Lead.Session != o.Session || o.OriginInput.Validate() != nil || o.OriginInput.Conversation != o.Lead || o.OriginInput.Action != "send" || !piUtilityHex.MatchString(o.Generation) || p.CreatedAt.IsZero() || p.Events == nil || validateStart(req) != nil || p.RequestDigest != digest(req) {
		return piruntime.ErrScope
	}
	if u.Kind != "verification_watcher" || !canonicalPath(u.PackageRoot) || !piUtilityHex.MatchString(u.CatalogDigest) || !piRecordText(u.Model, 4096) || !piRecordText(u.Effort, 64) || !piRecordText(u.Instructions, 64*1024) || u.MaxConcurrent < 1 || u.Settings.Validate() != nil || u.Settings.Model != u.Model || (u.SavedAccess != "read_only" && u.SavedAccess != "workspace_write") || (u.ExecutionAccess != "" && u.ExecutionAccess != "full_access") {
		return piruntime.ErrScope
	}
	if l.Utility.Validate(false) != nil || l.Utility.Session != o.Session || l.Utility.OperationID != p.ID || l.CreationID != derivedID(p.ID, "utility") || l.MessageID != derivedID(p.ID, "launch") || !piRecordText(l.Envelope, 40*1024) || l.Settings != u.Settings || !bounded(l.Detail, 4096) {
		return piruntime.ErrScope
	}
	switch l.State {
	case "reserved", "creating", "initialized", "submitting", "running", "held", "terminated":
	default:
		return piruntime.ErrScope
	}
	if l.State != "reserved" && !l.CreateAttempted {
		return piruntime.ErrScope
	}
	if (l.State == "initialized" || l.State == "submitting" || l.State == "running") && l.Utility.NativeID == "" {
		return piruntime.ErrHeld
	}
	if l.Input != nil && (l.Input.Validate() != nil || l.Input.Utility != l.Utility || l.Input.OperationID != l.MessageID || l.Input.Action != "send") {
		return piruntime.ErrScope
	}
	if (l.Outcome != "" && l.Outcome != "accepted" && l.Outcome != "not_submitted" && l.Outcome != "unknown") || (l.Outcome != "" && l.Input == nil) || (l.Generation != "" && !piUtilityHex.MatchString(l.Generation)) {
		return piruntime.ErrScope
	}
	if l.State == "running" && (l.Input == nil || l.Outcome != "accepted" || l.Generation == "") {
		return piruntime.ErrHeld
	}
	if l.State == "terminated" && l.TerminatedAt == nil {
		return piruntime.ErrHeld
	}
	seen := map[string]bool{}
	for _, e := range p.Events {
		if validateReport(e.Report) != nil || seen[e.Report.Slot] || e.ID != derivedID(p.ID, e.Report.Slot) || e.CreatedAt.IsZero() || e.Delivery.MessageID != e.ID || e.Delivery.Context != "monitor:"+p.ID+":"+e.ID || !bounded(e.Delivery.Envelope, 40*1024) || !bounded(e.Delivery.Detail, 4096) {
			return piruntime.ErrScope
		}
		seen[e.Report.Slot] = true
		switch e.Delivery.Status {
		case "pending", "held", "submitting", "accepted", "canceled":
		default:
			return piruntime.ErrScope
		}
		if e.Delivery.PiDelivery != nil && (e.Delivery.PiDelivery.Validate() != nil || e.Delivery.PiDelivery.Input.Conversation != o.Lead || e.Delivery.PiDelivery.Input.OperationID != e.ID) {
			return piruntime.ErrScope
		}
		if e.Delivery.Status == "accepted" && (e.Delivery.PiDelivery == nil || e.Delivery.PiDelivery.Outcome != "accepted") {
			return piruntime.ErrScope
		}
	}
	if p.Resolution != nil {
		r := p.Resolution
		if r.Lead != o.Lead || r.At.IsZero() || validateResolution(ResolutionRequest{OperationID: p.ID, ResolutionID: r.ID, Evidence: r.Evidence, Detail: r.Detail}) != nil {
			return piruntime.ErrScope
		}
	}
	if p.Settlement != nil {
		s := p.Settlement
		if (s.State != "incomplete" && s.State != "settled") || !piUtilityHex.MatchString(s.ObservedGeneration) || s.ObservedAt.IsZero() || len(s.SourceEvidence) > 16 || len(s.ToolEvidence) > 16 {
			return piruntime.ErrScope
		}
		for _, v := range append(append([]string{}, s.SourceEvidence...), s.ToolEvidence...) {
			if !piRecordText(v, 1024) {
				return piruntime.ErrScope
			}
		}
		if s.State == "settled" && (!seen["terminal"] || s.ReportEventID != derivedID(p.ID, "terminal") || len(s.SourceEvidence) == 0 || len(s.ToolEvidence) == 0 || l.Outcome != "accepted" || l.TerminatedAt == nil || l.State != "terminated") {
			return piruntime.ErrHeld
		}
	}
	return nil
}
func DecodePiOperation(data []byte) (PiOperation, error) {
	var p PiOperation
	err := DecodePiRecord(data, &p, maxOperationBytes)
	if err == nil {
		err = p.Validate()
	}
	return p, err
}

type legacyOperation Operation

func (op Operation) MarshalJSON() ([]byte, error) {
	if op.Pi != nil {
		return json.Marshal(op.Pi)
	}
	return json.Marshal(legacyOperation(op))
}
func (op *Operation) UnmarshalJSON(data []byte) error {
	var h struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	if h.Schema == 2 {
		p, err := DecodePiOperation(data)
		if err != nil {
			return err
		}
		*op = Operation{Schema: 2, ID: p.ID, Workspace: p.Workspace, Pi: &p}
		return nil
	}
	if h.Schema != 1 {
		return piruntime.ErrScope
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var legacy legacyOperation
	if err := d.Decode(&legacy); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return piruntime.ErrScope
	}
	*op = Operation(legacy)
	return nil
}
func operationSlug(op Operation) string {
	if op.Pi != nil {
		return op.Pi.Owner.Session.Slug
	}
	return op.Owner.Slug
}
func operationCapacity(op Operation) int {
	if op.Pi != nil {
		return op.Pi.UtilityPolicy.MaxConcurrent
	}
	return op.UtilityPolicy.MaxConcurrent
}
func operationActive(op Operation) bool {
	if op.Pi != nil {
		return op.Pi.Launch.TerminatedAt == nil || op.Pi.Settlement == nil || op.Pi.Settlement.State != "settled"
	}
	return op.Launch.TerminatedAt == nil && op.Disposition != "archived"
}
func validatePiTransition(old, next PiOperation) error {
	if next.Validate() != nil {
		return piruntime.ErrScope
	}
	// Only explicitly mutable state/proof projections are cleared for immutable
	// comparison; all original identities/settings/source/receipt refs remain.
	a, b := old, next
	a.Events, b.Events = nil, nil
	a.CanceledAt, b.CanceledAt = nil, nil
	a.Disposition, b.Disposition = "", ""
	a.Resolution, b.Resolution = nil, nil
	a.Settlement, b.Settlement = nil, nil
	a.Launch.State, b.Launch.State = "", ""
	a.Launch.CreateAttempted, b.Launch.CreateAttempted = false, false
	a.Launch.Utility.NativeID, b.Launch.Utility.NativeID = "", ""
	a.Launch.Input, b.Launch.Input = nil, nil
	a.Launch.Outcome, b.Launch.Outcome = "", ""
	a.Launch.Generation, b.Launch.Generation = "", ""
	a.Launch.TerminatedAt, b.Launch.TerminatedAt = nil, nil
	a.Launch.Termination, b.Launch.Termination = "", ""
	a.Launch.Detail, b.Launch.Detail = "", ""
	if !reflect.DeepEqual(a, b) || old.Launch.CreateAttempted && !next.Launch.CreateAttempted || old.Launch.Utility.NativeID != "" && old.Launch.Utility.NativeID != next.Launch.Utility.NativeID || old.Launch.Input != nil && !reflect.DeepEqual(old.Launch.Input, next.Launch.Input) || old.Launch.Generation != "" && old.Launch.Generation != next.Launch.Generation || (old.Launch.Outcome == "accepted" || old.Launch.Outcome == "not_submitted") && old.Launch.Outcome != next.Launch.Outcome || old.Launch.Outcome == "unknown" && next.Launch.Outcome == "" || old.CanceledAt != nil && !reflect.DeepEqual(old.CanceledAt, next.CanceledAt) || old.Resolution != nil && !reflect.DeepEqual(old.Resolution, next.Resolution) || old.Settlement != nil && old.Settlement.State == "settled" && !reflect.DeepEqual(old.Settlement, next.Settlement) || old.Launch.TerminatedAt != nil && !reflect.DeepEqual(old.Launch.TerminatedAt, next.Launch.TerminatedAt) || len(next.Events) < len(old.Events) {
		return piruntime.ErrConflict
	}
	for i, e := range old.Events {
		n := next.Events[i]
		a, b := e, n
		a.Delivery, b.Delivery = PiEventDelivery{}, PiEventDelivery{}
		if !reflect.DeepEqual(a, b) || e.Delivery.MessageID != n.Delivery.MessageID || e.Delivery.Envelope != n.Delivery.Envelope || e.Delivery.Context != n.Delivery.Context || ValidatePiDeliveryTransition(e.Delivery.PiDelivery, n.Delivery.PiDelivery) != nil {
			return piruntime.ErrConflict
		}
	}
	return nil
}
func (s *Store) ReservePi(ctx context.Context, admission PiTeamWriteAdmission, generation string, p PiOperation) (PiOperation, error) {
	var result PiOperation
	err := WithPiRecordWrite(ctx, admission, p.Owner.Session, generation, func() error {
		if p.Validate() != nil {
			return piruntime.ErrScope
		}
		encoded, _ := json.Marshal(p)
		// Reserve both event envelopes/receipts and later terminal proofs before
		// creation or Send can consume space needed for recovery.
		if len(encoded)+(2-len(p.Events))*(72*1024)+48*1024 > maxOperationBytes {
			return errors.New("Pi monitor recovery capacity exhausted")
		}
		op, err := s.reserve(ctx, Operation{Schema: 2, ID: p.ID, Workspace: p.Workspace, Pi: &p})
		if err == nil {
			result = *op.Pi
		}
		return err
	})
	return result, err
}
func (s *Store) UpdatePi(ctx context.Context, admission PiTeamWriteAdmission, scope PiSessionRecord, generation, id string, mutate func(*PiOperation) error) error {
	return WithPiRecordWrite(ctx, admission, scope, generation, func() error {
		tx, err := s.Lock(ctx, id)
		if err != nil {
			return err
		}
		defer tx.Close()
		if tx.Operation.Pi == nil || tx.Operation.Pi.Owner.Session != scope {
			return piruntime.ErrScope
		}
		if mutate == nil {
			return errors.New("missing Pi operation mutation")
		}
		if err := mutate(tx.Operation.Pi); err != nil {
			return err
		}
		tx.piWrite = true
		return tx.Save()
	})
}
