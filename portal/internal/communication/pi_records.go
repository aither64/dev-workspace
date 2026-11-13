package communication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type PiEndpoint struct {
	Backend         string                           `json:"backend"`
	Session         monitoroperation.PiSessionRecord `json:"session"`
	LeadIncarnation string                           `json:"leadIncarnation"`
	NativeID        string                           `json:"nativeId"`
}
type EndpointRecord struct {
	Codex *Identity
	Pi    *PiEndpoint
}

func (e EndpointRecord) MarshalJSON() ([]byte, error) {
	if e.Codex != nil && e.Pi == nil {
		return json.Marshal(e.Codex)
	}
	if e.Pi != nil && e.Codex == nil {
		return json.Marshal(e.Pi)
	}
	return nil, piruntime.ErrScope
}
func (e *EndpointRecord) UnmarshalJSON(data []byte) error {
	var h map[string]json.RawMessage
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	if _, pi := h["backend"]; pi {
		var p PiEndpoint
		if err := monitoroperation.DecodePiRecord(data, &p, 16*1024); err != nil {
			return err
		}
		*e = EndpointRecord{Pi: &p}
	} else {
		var c Identity
		if err := monitoroperation.DecodePiRecord(data, &c, 16*1024); err != nil {
			return err
		}
		*e = EndpointRecord{Codex: &c}
	}
	return e.Validate()
}
func (e EndpointRecord) Validate() error {
	if e.Codex != nil && e.Pi == nil {
		c := e.Codex
		if session.ValidSlug(c.Slug) && c.Thread != "" && c.Incarnation != "" {
			return nil
		}
	}
	if e.Pi != nil && e.Codex == nil {
		p := e.Pi
		if p.Backend == "pi" && p.Session.Validate() == nil && len(p.LeadIncarnation) == 64 && p.NativeID != "" && len(p.NativeID) <= 4096 {
			c := monitoroperation.PiConversationRecord{Session: p.Session, Backend: "pi", Member: "lead", Incarnation: p.LeadIncarnation, NativeID: p.NativeID}
			return c.Validate()
		}
	}
	return piruntime.ErrScope
}
func (e EndpointRecord) slug() string {
	if e.Pi != nil {
		return e.Pi.Session.Slug
	}
	if e.Codex != nil {
		return e.Codex.Slug
	}
	return ""
}
func (a EndpointRecord) equal(b EndpointRecord) bool { return reflect.DeepEqual(a, b) }

type PiGrantRecord struct {
	ID         string         `json:"id"`
	A          EndpointRecord `json:"a"`
	B          EndpointRecord `json:"b"`
	ApprovedBy string         `json:"approvedBy"`
	Approval   string         `json:"approval"`
	CreatedAt  time.Time      `json:"createdAt"`
	RevokedAt  *time.Time     `json:"revokedAt,omitempty"`
	Reason     string         `json:"reason,omitempty"`
}
type PiMessageRecord struct {
	ID         string                             `json:"id"`
	GrantID    string                             `json:"grantId"`
	From       EndpointRecord                     `json:"from"`
	To         EndpointRecord                     `json:"to"`
	Text       string                             `json:"text"`
	ReplyTo    string                             `json:"replyTo,omitempty"`
	CreatedAt  time.Time                          `json:"createdAt"`
	Status     string                             `json:"status"`
	Detail     string                             `json:"detail,omitempty"`
	Envelope   string                             `json:"envelope,omitempty"`
	Options    *codex.TurnOptions                 `json:"options,omitempty"`
	Receipt    *codex.SendReceipt                 `json:"receipt,omitempty"`
	PiDelivery *monitoroperation.PiDeliveryRecord `json:"piDelivery,omitempty"`
}
type PiCommunicationState struct {
	Schema    int               `json:"schema"`
	Workspace string            `json:"workspace"`
	Grants    []PiGrantRecord   `json:"grants"`
	Messages  []PiMessageRecord `json:"messages"`
}

func (p PiCommunicationState) Validate() error {
	if p.Schema != 2 || p.Workspace == "" || p.Grants == nil || p.Messages == nil {
		return piruntime.ErrScope
	}
	grants := map[string]PiGrantRecord{}
	for _, g := range p.Grants {
		if !messageIDPattern.MatchString(g.ID) || g.A.Validate() != nil || g.B.Validate() != nil || g.A.slug() == g.B.slug() || g.CreatedAt.IsZero() || g.Approval == "" || (g.ApprovedBy != g.A.slug() && g.ApprovedBy != g.B.slug()) {
			return piruntime.ErrScope
		}
		if _, exists := grants[g.ID]; exists {
			return piruntime.ErrConflict
		}
		grants[g.ID] = g
		for _, e := range []EndpointRecord{g.A, g.B} {
			if e.Pi != nil && e.Pi.Session.Workspace != p.Workspace {
				return piruntime.ErrScope
			}
		}
	}
	ids := map[string]bool{}
	for _, m := range p.Messages {
		g, ok := grants[m.GrantID]
		if !ok || ids[m.ID] || !messageIDPattern.MatchString(m.ID) || m.CreatedAt.IsZero() || m.Text == "" || len(m.Text) > session.MaxMessageBytes ||
			!((m.From.equal(g.A) && m.To.equal(g.B)) || (m.From.equal(g.B) && m.To.equal(g.A))) {
			return piruntime.ErrScope
		}
		ids[m.ID] = true
		switch m.Status {
		case "pending", "held", "canceled", "submitting", "delivered":
		default:
			return piruntime.ErrScope
		}
		if m.To.Pi != nil {
			if m.Options != nil || m.Receipt != nil {
				return piruntime.ErrScope
			}
			if m.PiDelivery != nil {
				d := m.PiDelivery
				if d.Validate() != nil || d.Input.OperationID != m.ID || d.Input.Conversation.Session != m.To.Pi.Session || d.Input.Conversation.Member != "lead" || d.Input.Conversation.NativeID != m.To.Pi.NativeID || d.Input.Conversation.Incarnation != m.To.Pi.LeadIncarnation {
					return piruntime.ErrScope
				}
			}
			if (m.Status == "submitting" || m.Status == "delivered") && (m.PiDelivery == nil || m.Envelope == "") {
				return piruntime.ErrScope
			}
			if m.Status == "delivered" && m.PiDelivery.Outcome != "accepted" {
				return piruntime.ErrScope
			}
		} else {
			if m.PiDelivery != nil {
				return piruntime.ErrScope
			}
			if (m.Status == "submitting" || m.Status == "delivered") && (m.Options == nil || m.Envelope == "") {
				return piruntime.ErrScope
			}
		}
	}
	return nil
}
func DecodePiCommunication(data []byte) (PiCommunicationState, error) {
	var p PiCommunicationState
	err := monitoroperation.DecodeMixedPiRecord(data, &p, maxStateBytes)
	if err == nil {
		err = p.Validate()
	}
	return p, err
}

type legacyState state

func (s *state) UnmarshalJSON(data []byte) error {
	var h struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}
	if h.Schema == 2 {
		p, err := DecodePiCommunication(data)
		if err != nil {
			return err
		}
		*s = state{Schema: 2, Workspace: p.Workspace, Pi: &p, Grants: []Grant{}, Messages: []Message{}}
		for _, g := range p.Grants {
			if g.A.Codex != nil && g.B.Codex != nil {
				s.Grants = append(s.Grants, Grant{ID: g.ID, A: *g.A.Codex, B: *g.B.Codex, ApprovedBy: g.ApprovedBy, Approval: g.Approval, CreatedAt: g.CreatedAt, RevokedAt: g.RevokedAt, Reason: g.Reason})
			}
		}
		for _, m := range p.Messages {
			if m.From.Codex != nil && m.To.Codex != nil {
				s.Messages = append(s.Messages, Message{ID: m.ID, GrantID: m.GrantID, From: *m.From.Codex, To: *m.To.Codex, Text: m.Text, ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt, Status: m.Status, Detail: m.Detail, Envelope: m.Envelope, Options: m.Options, Receipt: m.Receipt})
			}
		}
		return nil
	}
	if h.Schema != 1 {
		return piruntime.ErrScope
	}
	var l legacyState
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&l); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return piruntime.ErrScope
	}
	*s = state(l)
	return nil
}
func (s *state) aggregate() PiCommunicationState {
	p := PiCommunicationState{Schema: 2, Workspace: s.Workspace, Grants: []PiGrantRecord{}, Messages: []PiMessageRecord{}}
	if s.Pi != nil {
		for _, g := range s.Pi.Grants {
			if g.A.Pi != nil || g.B.Pi != nil {
				p.Grants = append(p.Grants, g)
			}
		}
		for _, m := range s.Pi.Messages {
			if m.From.Pi != nil || m.To.Pi != nil {
				p.Messages = append(p.Messages, m)
			}
		}
	}
	for _, g := range s.Grants {
		a, b := g.A, g.B
		p.Grants = append(p.Grants, PiGrantRecord{ID: g.ID, A: EndpointRecord{Codex: &a}, B: EndpointRecord{Codex: &b}, ApprovedBy: g.ApprovedBy, Approval: g.Approval, CreatedAt: g.CreatedAt, RevokedAt: g.RevokedAt, Reason: g.Reason})
	}
	for _, m := range s.Messages {
		a, b := m.From, m.To
		p.Messages = append(p.Messages, PiMessageRecord{ID: m.ID, GrantID: m.GrantID, From: EndpointRecord{Codex: &a}, To: EndpointRecord{Codex: &b}, Text: m.Text, ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt, Status: m.Status, Detail: m.Detail, Envelope: m.Envelope, Options: m.Options, Receipt: m.Receipt})
	}
	return p
}
func (s state) MarshalJSON() ([]byte, error) {
	if s.Schema == 2 {
		p := s.aggregate()
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return json.Marshal(p)
	}
	return json.Marshal(legacyState(s))
}

// UpdatePi upgrades the existing aggregate once under its existing lock. The
// new writer admission runs first; no legacy message is reapproved or resent.
func (s *Store) UpdatePi(ctx context.Context, admission monitoroperation.PiTeamWriteAdmission, scope monitoroperation.PiSessionRecord, generation string, mutate func(*PiCommunicationState) error) error {
	return monitoroperation.WithPiRecordWrite(ctx, admission, scope, generation, func() error {
		return s.lockedAdmitted(ctx, true, true, func(current *state) error {
			next := current.aggregate()
			old, _ := json.Marshal(next)
			if mutate == nil {
				return errors.New("missing Pi communication mutation")
			}
			if err := mutate(&next); err != nil {
				return err
			}
			if err := next.Validate(); err != nil {
				return err
			}
			if err := validatePiCommunicationTransition(old, next); err != nil {
				return err
			}
			// Decode through the same codec to refresh both legacy projection and
			// Pi arms before saving; otherwise a stale projection could erase work.
			data, _ := json.Marshal(next)
			var merged state
			if err := json.Unmarshal(data, &merged); err != nil {
				return err
			}
			*current = merged
			return s.admit(current)
		})
	})
}
func validatePiCommunicationTransition(old []byte, next PiCommunicationState) error {
	var prior PiCommunicationState
	if err := json.Unmarshal(old, &prior); err != nil {
		return err
	}
	if prior.Workspace != next.Workspace || len(next.Grants) < len(prior.Grants) || len(next.Messages) < len(prior.Messages) {
		return piruntime.ErrConflict
	}
	for _, g := range prior.Grants {
		found := false
		for _, n := range next.Grants {
			if g.ID == n.ID {
				a, b := g, n
				a.RevokedAt, b.RevokedAt = nil, nil
				a.Reason, b.Reason = "", ""
				if !reflect.DeepEqual(a, b) || g.RevokedAt != nil && !reflect.DeepEqual(g, n) {
					return piruntime.ErrConflict
				}
				found = true
			}
		}
		if !found {
			return piruntime.ErrConflict
		}
	}
	for _, m := range prior.Messages {
		found := false
		for _, n := range next.Messages {
			if m.ID == n.ID {
				if m.To.Codex != nil {
					if !reflect.DeepEqual(m, n) {
						return piruntime.ErrConflict
					}
				} else {
					a, b := m, n
					a.Status, b.Status = "", ""
					a.Detail, b.Detail = "", ""
					a.Envelope, b.Envelope = "", ""
					a.PiDelivery, b.PiDelivery = nil, nil
					if !reflect.DeepEqual(a, b) || m.Envelope != "" && m.Envelope != n.Envelope || monitoroperation.ValidatePiDeliveryTransition(m.PiDelivery, n.PiDelivery) != nil ||
						(m.Status == "delivered" && n.Status != "delivered") || m.PiDelivery != nil && m.PiDelivery.Outcome != "accepted" && n.Status == "canceled" {
						return piruntime.ErrConflict
					}
				}
				found = true
			}
		}
		if !found {
			return piruntime.ErrConflict
		}
	}
	return nil
}
