package communication

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// EndpointView preserves legacy Codex JSON while retaining both typed arms.
// Private delivery tuples never leave this owner through a public view.
type EndpointView struct {
	Grants   []PiGrantRecord   `json:"grants"`
	Messages []PiMessageRecord `json:"messages"`
}

// Public receipts deliberately omit native options/envelopes/receipt payloads;
// they cannot be validated as a durable state requiring those private tuples.
func (v EndpointView) ValidatePublic() error {
	if v.Grants == nil {
		return piruntime.ErrScope
	}
	grants := map[string]PiGrantRecord{}
	for _, g := range v.Grants {
		if !messageIDPattern.MatchString(g.ID) || g.A.Validate() != nil || g.B.Validate() != nil || g.A.slug() == g.B.slug() || strings.TrimSpace(g.Approval) == "" || len(g.Approval) > 4096 || !utf8.ValidString(g.Approval) || g.CreatedAt.IsZero() || g.ApprovedBy != g.A.slug() && g.ApprovedBy != g.B.slug() {
			return piruntime.ErrScope
		}
		if _, ok := grants[g.ID]; ok {
			return piruntime.ErrConflict
		}
		grants[g.ID] = g
	}
	ids := map[string]bool{}
	for _, m := range v.Messages {
		g, ok := grants[m.GrantID]
		if !ok || ids[m.ID] || !messageIDPattern.MatchString(m.ID) || m.From.Validate() != nil || m.To.Validate() != nil || m.CreatedAt.IsZero() || len(m.Text) > session.MaxMessageBytes || !utf8.ValidString(m.Text) || m.Envelope != "" || m.Options != nil || m.Receipt != nil || m.PiDelivery != nil || !((m.From.equal(g.A) && m.To.equal(g.B)) || (m.From.equal(g.B) && m.To.equal(g.A))) {
			return piruntime.ErrScope
		}
		switch m.Status {
		case "pending", "held", "submitting", "delivered", "canceled":
		default:
			return piruntime.ErrScope
		}
		ids[m.ID] = true
	}
	return nil
}

type PiPreparation struct {
	Settings   monitoroperation.PiSettingsRecord
	Generation string
}

// These are observational/admitted ports of the existing serving portal, not
// a CLI worker or a native tool-caller authentication interface.
type PiTransport interface {
	Prepare(context.Context, EndpointRecord) (PiPreparation, error)
	Send(context.Context, PiMessageRecord) (backend.SendResult, piruntime.SubmissionStatus, error)
	Lookup(context.Context, PiMessageRecord) (piruntime.SubmissionStatus, error)
}

func EndpointFor(workspace, stateRoot, slug string, archived bool) (EndpointRecord, error) {
	binding, err := (session.BackendBindingStore{Root: stateRoot, Workspace: workspace}).Load(slug)
	if errors.Is(err, agentbackend.ErrNotBound) {
		identity, err := IdentityFor(workspace, slug, archived)
		return EndpointRecord{Codex: &identity}, err
	}
	if err != nil {
		return EndpointRecord{}, err
	}
	summary, err := session.Find(workspace, slug)
	if err != nil || summary.Archived && !archived || summary.Codex.ThreadID != "" {
		return EndpointRecord{}, piruntime.ErrScope
	}
	if !archived {
		pending, err := session.PendingLifecycle(workspace, slug)
		if err != nil || pending != "" {
			return EndpointRecord{}, errors.Join(piruntime.ErrHeld, err)
		}
	}
	lead, err := binding.Resolve("lead")
	if err != nil {
		return EndpointRecord{}, err
	}
	endpoint := EndpointRecord{Pi: &PiEndpoint{Backend: "pi", Session: monitoroperation.PiSessionRecord{Workspace: workspace, Slug: slug, Incarnation: binding.SessionIncarnation}, LeadIncarnation: lead.Incarnation, NativeID: lead.NativeID}}
	return endpoint, endpoint.Validate()
}

func (e EndpointRecord) Slug() string { return e.slug() }
func (e EndpointRecord) Conversation() agentbackend.ConversationRef {
	if e.Pi == nil {
		return agentbackend.ConversationRef{}
	}
	return agentbackend.ConversationRef{Session: e.Pi.Session.Ref(), Backend: agentbackend.Pi, Member: "lead", Incarnation: e.Pi.LeadIncarnation, NativeID: e.Pi.NativeID}
}

func (m PiMessageRecord) legacy() Message {
	var from, to Identity
	if m.From.Codex != nil {
		from = *m.From.Codex
	}
	if m.To.Codex != nil {
		to = *m.To.Codex
	}
	return Message{ID: m.ID, GrantID: m.GrantID, From: from, To: to, Text: m.Text, ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt, Status: m.Status, Detail: m.Detail, Envelope: m.Envelope, Options: m.Options, Receipt: m.Receipt}
}
func (v EndpointView) legacy() View {
	result := View{Grants: []Grant{}, Messages: []Message{}}
	for _, g := range v.Grants {
		if g.A.Codex != nil && g.B.Codex != nil {
			result.Grants = append(result.Grants, Grant{ID: g.ID, A: *g.A.Codex, B: *g.B.Codex, ApprovedBy: g.ApprovedBy, Approval: g.Approval, CreatedAt: g.CreatedAt, RevokedAt: g.RevokedAt, Reason: g.Reason})
		}
	}
	for _, m := range v.Messages {
		if m.From.Codex != nil && m.To.Codex != nil {
			result.Messages = append(result.Messages, m.legacy())
		}
	}
	return result
}

func (st *state) replaceAggregate(p PiCommunicationState) error {
	if err := p.Validate(); err != nil {
		return err
	}
	hasPi := st.Schema == 2
	for _, g := range p.Grants {
		hasPi = hasPi || g.A.Pi != nil || g.B.Pi != nil
	}
	if hasPi {
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, st)
	}
	v := (EndpointView{Grants: p.Grants, Messages: p.Messages}).legacy()
	st.Grants, st.Messages = v.Grants, v.Messages
	return nil
}

func (s Service) resolveEndpoint(slug string) (EndpointRecord, error) {
	if s.ResolveEndpoint != nil {
		return s.ResolveEndpoint(slug)
	}
	if s.Resolve == nil {
		return EndpointRecord{}, errors.New("communication identity resolver unavailable")
	}
	identity, err := s.Resolve(slug)
	return EndpointRecord{Codex: &identity}, err
}

func (s Service) ReadEndpoint(ctx context.Context, caller EndpointRecord) (EndpointView, error) {
	view := EndpointView{Grants: []PiGrantRecord{}, Messages: []PiMessageRecord{}}
	if caller.Validate() != nil {
		return view, piruntime.ErrScope
	}
	err := s.Store.locked(ctx, false, func(st *state) error {
		p := st.aggregate()
		for _, g := range p.Grants {
			if g.A.equal(caller) || g.B.equal(caller) {
				view.Grants = append(view.Grants, g)
			}
		}
		for _, m := range p.Messages {
			if m.From.equal(caller) || m.To.equal(caller) {
				m.Envelope = ""
				m.Options = nil
				m.Receipt = nil
				m.PiDelivery = nil
				view.Messages = append(view.Messages, m)
			}
		}
		return nil
	})
	slices.SortStableFunc(view.Messages, func(a, b PiMessageRecord) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return view, err
}

func validateRequest(caller EndpointRecord, req Request) error {
	if caller.Validate() != nil {
		return piruntime.ErrScope
	}
	if req.Action != "allow" && req.Action != "revoke" && req.Action != "send" && req.Action != "reply" {
		return errors.New("unknown communication operation")
	}
	if req.Peer == caller.slug() {
		return errors.New("choose another session")
	}
	if req.Action == "allow" && (strings.TrimSpace(req.Approval) == "" || len(req.Approval) > 4096 || !utf8.ValidString(req.Approval)) {
		return errors.New("explicit user approval and its source are required")
	}
	if req.Action == "send" || req.Action == "reply" {
		if !messageIDPattern.MatchString(req.MessageID) || strings.TrimSpace(req.Message) == "" || len(req.Message) > session.MaxMessageBytes || !utf8.ValidString(req.Message) || strings.ContainsRune(req.Message, 0) {
			return errors.New("provide a stable message ID and nonempty text within the message limit")
		}
		if req.Action == "reply" && req.ReplyTo == "" {
			return errors.New("reply requires a message reference")
		}
	}
	return nil
}

func (s Service) writeEndpoints(ctx context.Context, mixed bool, run func(*state) error) error {
	if !mixed {
		return s.Store.locked(ctx, true, run)
	}
	if s.Store.Schema2Write == nil {
		return errors.New("Pi communication writer admission unavailable")
	}
	called := false
	var callbackErr error
	err := s.Store.Schema2Write(ctx, func() error {
		if called {
			callbackErr = piruntime.ErrConflict
			return callbackErr
		}
		called = true
		callbackErr = s.Store.lockedAdmitted(ctx, true, true, run)
		return callbackErr
	})
	if callbackErr != nil {
		return callbackErr
	}
	if err == nil && !called {
		return piruntime.ErrUnavailable
	}
	return err
}

// Current endpoint/source admission always applies. Delivery readiness is a
// separate existing port: removing authorization does not enqueue any input.
func (s Service) authorizeEndpoint(ctx context.Context, endpoint EndpointRecord, delivery bool) error {
	if s.AuthorizeEndpoint != nil {
		if err := s.AuthorizeEndpoint(ctx, endpoint); err != nil {
			return err
		}
	} else if endpoint.Pi != nil {
		return piruntime.ErrUnavailable
	}
	if delivery && endpoint.Pi != nil {
		if s.Pi == nil {
			return piruntime.ErrUnavailable
		}
		_, err := s.Pi.Prepare(ctx, endpoint)
		return err
	}
	return nil
}

func (s Service) RunEndpoint(ctx context.Context, caller EndpointRecord, req Request) (EndpointView, error) {
	if req.Action == "list" || req.Action == "history" {
		return s.ReadEndpoint(ctx, caller)
	}
	if err := validateRequest(caller, req); err != nil {
		return EndpointView{}, err
	}
	unlock, err := s.lockSessions(ctx, caller.slug(), req.Peer)
	if err != nil {
		return EndpointView{}, err
	}
	defer unlock()
	current, err := s.resolveEndpoint(caller.slug())
	if err != nil || !current.equal(caller) {
		return EndpointView{}, errors.New("calling session identity changed")
	}
	peer, err := s.resolveEndpoint(req.Peer)
	if err != nil {
		return EndpointView{}, err
	}
	for _, endpoint := range []EndpointRecord{caller, peer} {
		if err := s.authorizeEndpoint(ctx, endpoint, req.Action != "revoke"); err != nil {
			return EndpointView{}, err
		}
	}
	err = s.writeEndpoints(ctx, caller.Pi != nil || peer.Pi != nil, func(st *state) error {
		p := st.aggregate()
		old, _ := json.Marshal(p)
		var grant *PiGrantRecord
		for i := range p.Grants {
			g := &p.Grants[i]
			if g.RevokedAt == nil && ((g.A.equal(caller) && g.B.equal(peer)) || (g.B.equal(caller) && g.A.equal(peer))) {
				grant = g
				break
			}
		}
		if req.Action == "allow" {
			if grant != nil {
				return nil
			}
			p.Grants = append(p.Grants, PiGrantRecord{ID: newID(), A: caller, B: peer, ApprovedBy: caller.slug(), Approval: req.Approval, CreatedAt: time.Now().UTC()})
		} else if req.Action == "revoke" {
			if grant != nil {
				revokeEndpoint(&p, grant, "Revoked by user", time.Now().UTC())
			}
		} else {
			for _, m := range p.Messages {
				if m.ID == req.MessageID {
					if !m.From.equal(caller) || !m.To.equal(peer) || m.Text != req.Message || m.ReplyTo != req.ReplyTo {
						return errors.New("message ID was already used with different content")
					}
					return nil
				}
			}
			if grant == nil {
				return errors.New("user approval is required for this session pair")
			}
			if req.ReplyTo != "" {
				found := false
				for _, m := range p.Messages {
					if m.ID == req.ReplyTo && m.GrantID == grant.ID {
						found = true
						break
					}
				}
				if !found {
					return errors.New("reply reference is outside this approved exchange")
				}
			}
			message := PiMessageRecord{ID: req.MessageID, GrantID: grant.ID, From: caller, To: peer, Text: req.Message, ReplyTo: req.ReplyTo, CreatedAt: time.Now().UTC(), Status: "pending"}
			if caller.Codex != nil && peer.Codex != nil {
				message.Envelope = envelope(*caller.Codex, *peer.Codex, message.ID, message.Text)
			}
			p.Messages = append(p.Messages, message)
		}
		if caller.Pi != nil || peer.Pi != nil {
			if err := validatePiCommunicationTransition(old, p); err != nil {
				return err
			}
		}
		if err := st.replaceAggregate(p); err != nil {
			return err
		}
		if req.Action == "revoke" {
			return s.Store.save(st)
		}
		return s.Store.admit(st)
	})
	if err != nil {
		return EndpointView{}, err
	}
	return s.ReadEndpoint(ctx, caller)
}

func revokeEndpoint(st *PiCommunicationState, g *PiGrantRecord, reason string, at time.Time) {
	g.RevokedAt = &at
	g.Reason = reason
	for i := range st.Messages {
		m := &st.Messages[i]
		if m.GrantID != g.ID || m.Status != "pending" && m.Status != "held" {
			continue
		}
		if m.To.Pi != nil && m.PiDelivery != nil || m.From.Pi != nil && m.To.Codex != nil && m.Options != nil {
			continue
		}
		m.Status = "canceled"
		m.Detail = reason
	}
}

func (s Service) dispatchEndpoint(ctx context.Context, id string) error {
	var original PiMessageRecord
	err := s.Store.locked(ctx, false, func(st *state) error {
		for _, m := range st.aggregate().Messages {
			if m.ID == id {
				original = m
				return nil
			}
		}
		return errors.New("message does not exist")
	})
	if err != nil {
		return err
	}
	unlock, err := s.lockSessions(ctx, original.From.slug(), original.To.slug())
	if err != nil {
		return err
	}
	defer unlock()
	mixed := original.From.Pi != nil || original.To.Pi != nil
	err = s.writeEndpoints(ctx, mixed, func(st *state) error {
		p := st.aggregate()
		old, _ := json.Marshal(p)
		var m *PiMessageRecord
		for i := range p.Messages {
			if p.Messages[i].ID == id {
				m = &p.Messages[i]
				break
			}
		}
		if m == nil || !m.From.equal(original.From) || !m.To.equal(original.To) {
			return piruntime.ErrConflict
		}
		if m.Status == "delivered" || m.Status == "canceled" {
			return nil
		}
		var grant *PiGrantRecord
		for i := range p.Grants {
			if p.Grants[i].ID == m.GrantID {
				grant = &p.Grants[i]
				break
			}
		}
		from, fromErr := s.resolveEndpoint(m.From.slug())
		to, toErr := s.resolveEndpoint(m.To.slug())
		allowed := grant != nil && grant.RevokedAt == nil && ((grant.A.equal(m.From) && grant.B.equal(m.To)) || (grant.B.equal(m.From) && grant.A.equal(m.To))) && fromErr == nil && toErr == nil && from.equal(m.From) && to.equal(m.To)
		save := func() error {
			if mixed {
				if err := validatePiCommunicationTransition(old, p); err != nil {
					return err
				}
			}
			if err := st.replaceAggregate(p); err != nil {
				return err
			}
			if err := s.Store.save(st); err != nil {
				return err
			}
			old, _ = json.Marshal(p)
			return nil
		}
		if m.To.Pi != nil && m.PiDelivery != nil {
			return errPiLookupRequired
		}
		if m.To.Codex != nil && (m.Status == "submitting" || m.From.Pi != nil && m.Options != nil) {
			if s.Transport == nil {
				return errors.New("communication transport unavailable")
			}
			receipt, found, err := s.Transport.Reconcile(ctx, m.legacy())
			if found && err == nil {
				legacy := m.legacy()
				confirmDelivery(&legacy, receipt)
				m.Status, m.Detail, m.Receipt = legacy.Status, legacy.Detail, legacy.Receipt
				return save()
			}
			if err != nil || m.From.Pi != nil {
				m.Detail = "Delivery is unconfirmed; reconciliation will retry"
				return save()
			}
		}
		if !allowed {
			m.Status = "canceled"
			m.Detail = "Approval or session identity is no longer valid"
			return save()
		}
		for _, endpoint := range []EndpointRecord{from, to} {
			if err := s.authorizeEndpoint(ctx, endpoint, true); err != nil {
				m.Status = "held"
				m.Detail = "Session admission is unavailable"
				return save()
			}
		}
		if m.To.Pi != nil {
			if s.Pi == nil {
				return piruntime.ErrUnavailable
			}
			prepared, err := s.Pi.Prepare(ctx, m.To)
			if err != nil || prepared.Settings.Validate() != nil || !communicationGeneration.MatchString(prepared.Generation) {
				m.Status = "held"
				m.Detail = "Recipient is held or unavailable"
				return save()
			}
			if m.Envelope == "" {
				m.Envelope, err = endpointEnvelope(*grant, *m)
				if err != nil {
					return err
				}
			}
			input := monitoroperation.PiInputRecord{Conversation: monitoroperation.PiConversationRecord{Session: m.To.Pi.Session, Backend: "pi", Member: "lead", Incarnation: m.To.Pi.LeadIncarnation, NativeID: m.To.Pi.NativeID}, OperationID: m.ID, Action: "send"}
			input.Digest = piruntime.SubmissionDigest(input.Ref(), piruntime.SendInput{Message: m.Envelope, ActionContext: "pi-delivery-v1:steer"}, prepared.Settings.Runtime())
			m.PiDelivery = &monitoroperation.PiDeliveryRecord{Settings: prepared.Settings, Input: input, Generation: prepared.Generation}
			m.Status = "submitting"
			m.Detail = ""
			if err := save(); err != nil {
				return err
			}
			result, status, err := s.Pi.Send(ctx, *m)
			outcome := "unknown"
			if err == nil && result.Outcome != backend.Pending {
				expected, _ := expectedPiInput(*m)
				outcome = piReceiptOutcome(*m, expected, status)
			}
			m.PiDelivery.Outcome = outcome
			if outcome == "accepted" {
				m.Status = "delivered"
			} else {
				m.Status = "held"
				m.Detail = "Delivery is unconfirmed"
			}
			return save()
		}
		if s.Transport == nil {
			return errors.New("communication transport unavailable")
		}
		reason, err := s.Transport.Ready(ctx, *m.To.Codex)
		if err != nil {
			reason = "Recipient status is unavailable"
		}
		if reason != "" {
			m.Status = "held"
			m.Detail = reason
			return save()
		}
		if m.Options == nil {
			options, err := s.Transport.Prepare(ctx, *m.To.Codex)
			if err == nil {
				options, err = withEndpointAuthorizationContext(options, *grant, *m)
			}
			if err != nil {
				m.Status = "held"
				m.Detail = "Recipient settings are unavailable"
				return save()
			}
			encoded, err := json.Marshal(options)
			if err != nil || len(encoded) > maxOptionsBytes {
				m.Status = "held"
				m.Detail = "Recipient settings exceed the delivery limit"
				return save()
			}
			m.Options = &options
		}
		if m.Envelope == "" {
			m.Envelope, err = endpointEnvelope(*grant, *m)
			if err != nil {
				return err
			}
		}
		m.Status = "submitting"
		m.Detail = ""
		if err := save(); err != nil {
			return err
		}
		receipt, err := s.Transport.Send(ctx, m.legacy())
		if err != nil {
			m.Detail = "Delivery is unconfirmed; reconciliation will retry"
		} else {
			legacy := m.legacy()
			confirmDelivery(&legacy, receipt)
			m.Status, m.Detail, m.Receipt = legacy.Status, legacy.Detail, legacy.Receipt
		}
		return save()
	})
	if errors.Is(err, errPiLookupRequired) {
		return s.reconcileCapturedPi(ctx, id)
	}
	return err
}

var errPiLookupRequired = errors.New("original Pi input requires lookup")

// Read/lookup ends before the CAS writer. Pair ownership remains held by
// dispatchEndpoint; this never re-enters a runtime gate while holding the store.
func (s Service) reconcileCapturedPi(ctx context.Context, id string) error {
	var original PiMessageRecord
	if err := s.Store.locked(ctx, false, func(st *state) error {
		for _, m := range st.aggregate().Messages {
			if m.ID == id {
				original = m
				return nil
			}
		}
		return piruntime.ErrScope
	}); err != nil {
		return err
	}
	if original.Status == "delivered" || original.PiDelivery != nil && original.PiDelivery.Outcome == "not_submitted" {
		return nil
	}
	expected, proofErr := expectedPiInput(original)
	outcome := "unknown"
	if proofErr == nil && s.Pi != nil {
		status, err := s.Pi.Lookup(ctx, original)
		if err == nil {
			outcome = piReceiptOutcome(original, expected, status)
		}
	}
	return s.writeEndpoints(ctx, true, func(st *state) error {
		p := st.aggregate()
		old, _ := json.Marshal(p)
		for i := range p.Messages {
			m := &p.Messages[i]
			if m.ID != id {
				continue
			}
			if !reflect.DeepEqual(*m, original) {
				return piruntime.ErrConflict
			}
			if m.PiDelivery == nil {
				return piruntime.ErrConflict
			}
			m.PiDelivery.Outcome = outcome
			m.Status = "held"
			m.Detail = "Delivery is unconfirmed"
			if outcome == "accepted" {
				m.Status = "delivered"
				m.Detail = ""
			}
			if err := validatePiCommunicationTransition(old, p); err != nil {
				return err
			}
			if err := st.replaceAggregate(p); err != nil {
				return err
			}
			return s.Store.save(st)
		}
		return piruntime.ErrScope
	})
}
