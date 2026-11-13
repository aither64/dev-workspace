package monitoroperation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"time"
)

var ErrPollNotDue = errors.New("CI observation is not due or has unresolved local work")

type NativeStatus struct {
	Operation
	ExecutionOwner string `json:"executionOwner"`
}

type ObserverStatus struct {
	ObserverRecord
	ExecutionOwner string `json:"executionOwner"`
}

func (s *Service) AcknowledgeNative(ctx context.Context, op Operation) (NativeStatus, error) {
	return NativeStatus{Operation: op, ExecutionOwner: "native"}, nil
}

// StartManaged chooses a driver only after checking both retained formats.
// Existing CI operations keep their original native launch/retry contract.
func (s *Service) StartManaged(ctx context.Context, caller string, req StartRequest) (any, error) {
	if s.Store == nil {
		return nil, errors.New("monitor store is unavailable")
	}
	if err := validateStart(req); err != nil {
		return nil, err
	}
	if record, err := s.Store.GetObserver(ctx, req.ID); err == nil {
		if record.Driver == "direct-ci" {
			record, err = s.StartCIObservation(ctx, caller, req)
			return ObserverStatus{record, "runtime-ci"}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := s.Store.Get(ctx, req.ID); err == nil {
		op, err := s.Start(ctx, caller, req)
		if err != nil {
			return nil, err
		}
		return s.AcknowledgeNative(ctx, op)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if req.Source.Kind == "ci" {
		record, err := s.StartCIObservation(ctx, caller, req)
		return ObserverStatus{record, "runtime-ci"}, err
	}
	op, err := s.Start(ctx, caller, req)
	if err != nil {
		return nil, err
	}
	return s.AcknowledgeNative(ctx, op)
}

func (s *Service) StartCIObservation(ctx context.Context, caller string, req StartRequest) (ObserverRecord, error) {
	if s.Store == nil || s.ResolveLead == nil || s.Resolve == nil || s.LockSession == nil || s.Admit == nil {
		return ObserverRecord{}, errors.New("CI observation admission is unavailable")
	}
	if err := validateStart(req); err != nil {
		return ObserverRecord{}, err
	}
	if req.Source.Kind != "ci" || req.Source.Provider != "github" {
		return ObserverRecord{}, errors.New("direct CI observation supports only an existing GitHub run")
	}
	owner, err := s.ResolveLead(ctx, caller)
	if err != nil {
		return ObserverRecord{}, err
	}
	unlock, err := s.LockSession(ctx, owner.Slug)
	if err != nil {
		return ObserverRecord{}, err
	}
	defer unlock()
	current, err := s.Resolve(owner.Slug)
	if err != nil || !SameOwner(owner, current) || owner.SocketEpoch != current.SocketEpoch || caller != owner.RootThreadID || owner.OriginTurnID == "" {
		return ObserverRecord{}, errors.New("CI observer lead changed")
	}
	if err := s.Admit(ctx, current); err != nil {
		return ObserverRecord{}, err
	}
	tx, err := s.Store.lockObserver(ctx, req.ID, true)
	if err != nil {
		return ObserverRecord{}, err
	}
	defer tx.close()
	if tx.native != nil {
		return ObserverRecord{}, errors.New("monitor ID retains its native driver")
	}
	if tx.record != nil {
		if tx.record.RequestDigest != digest(req) || !SameOwner(tx.record.Owner, owner) || tx.record.Driver != "direct-ci" {
			return ObserverRecord{}, errors.New("monitor ID was reused for another request or driver")
		}
		return cloneObserver(*tx.record), nil
	}
	if err := ValidateDirectCISource(req.Source); err != nil {
		return ObserverRecord{}, err
	}
	at := s.now()
	tx.record = &ObserverRecord{Schema: ObserverSchema, ID: req.ID, Workspace: s.Store.workspace, Owner: owner, Driver: "direct-ci", RequestDigest: digest(req), Source: req.Source, SourceDigest: SourceDigest(req.Source), CreatedAt: at, UpdatedAt: at, Revision: 1, Phase: "observing", Events: []Event{}}
	if err := tx.save(); err != nil {
		return ObserverRecord{}, err
	}
	return cloneObserver(*tx.record), nil
}

func (s *Service) ObserverStatus(ctx context.Context, caller, id string) (ObserverRecord, error) {
	if !validID(id) {
		return ObserverRecord{}, errors.New("CI observer status requires exact ID")
	}
	record, err := s.Store.GetObserver(ctx, id)
	if err != nil {
		return record, err
	}
	owner, err := s.ResolveLead(ctx, caller)
	if err != nil {
		return record, err
	}
	unlock, err := s.LockSession(ctx, owner.Slug)
	if err != nil {
		return record, err
	}
	defer unlock()
	current, err := s.Resolve(owner.Slug)
	if err != nil || caller != record.Owner.RootThreadID || !SameOwner(owner, record.Owner) || !SameOwner(owner, current) {
		return ObserverRecord{}, errors.New("CI observer status owner changed")
	}
	return s.Store.GetObserver(ctx, id)
}

func (s *Service) CancelCIObservation(ctx context.Context, caller, id string) (ObserverRecord, error) {
	record, err := s.ObserverStatus(ctx, caller, id)
	if err != nil {
		return record, err
	}
	unlock, err := s.LockSession(ctx, record.Owner.Slug)
	if err != nil {
		return record, err
	}
	defer unlock()
	current, err := s.Resolve(record.Owner.Slug)
	if err != nil || !SameOwner(record.Owner, current) || current.RootThreadID != caller {
		return record, errors.New("CI observer cancellation owner changed")
	}
	tx, err := s.Store.lockObserver(ctx, id, false)
	if err != nil {
		return record, err
	}
	defer tx.close()
	if tx.record.Driver != "direct-ci" {
		return record, errors.New("native observer uses native cancellation")
	}
	if tx.record.CanceledAt == nil {
		now := s.now()
		tx.record.CanceledAt = &now
		cancelObserverEvents(tx.record)
		bumpObserver(tx.record, now)
		if err := tx.save(); err != nil {
			return record, err
		}
	}
	return cloneObserver(*tx.record), nil
}

func bumpObserver(record *ObserverRecord, at time.Time) {
	record.Revision++
	record.UpdatedAt = at.UTC()
}
func cancelObserverEvents(record *ObserverRecord) {
	for i := range record.Events {
		d := &record.Events[i].Delivery
		if d.Status != "accepted" && d.Status != "submitting" {
			d.Status = "canceled"
		}
	}
}

// ClaimCIPoll is the only admission for a new owned read. Receipt completion is
// metadata reconciliation and never launches another call.
func (s *Service) ClaimCIPoll(ctx context.Context, id string) (ObserverRecord, error) {
	if s.ObserverEpoch == "" || !bounded(s.ObserverEpoch, 4096) {
		return ObserverRecord{}, errors.New("CI poll requires its execution owner's epoch")
	}
	record, err := s.Store.GetObserver(ctx, id)
	if err != nil {
		return record, err
	}
	unlock, err := s.LockSession(ctx, record.Owner.Slug)
	if err != nil {
		return record, err
	}
	defer unlock()
	owner, err := s.Resolve(record.Owner.Slug)
	if err != nil || !SameOwner(owner, record.Owner) {
		return record, errors.New("CI poll owner changed")
	}
	if err := s.Admit(ctx, owner); err != nil {
		return record, err
	}
	tx, err := s.Store.lockObserver(ctx, id, false)
	if err != nil {
		return record, err
	}
	defer tx.close()
	r := tx.record
	now := s.now()
	if r.Driver != "direct-ci" || r.Phase != "observing" || len(r.Events) != 0 || r.Poll != nil && (r.Poll.State != "joined" || now.Sub(r.Poll.StartedAt) < 15*time.Second) {
		return record, ErrPollNotDue
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return record, err
	}
	r.Poll = &PollReceipt{ID: hex.EncodeToString(nonce[:]), OwnerEpoch: s.ObserverEpoch, State: "intent", StartedAt: now}
	bumpObserver(r, now)
	if err := tx.save(); err != nil {
		return record, err
	}
	return cloneObserver(*r), nil
}

func (s *Service) CommitCIPoll(ctx context.Context, claim ObserverRecord, snapshot *CISnapshot, receipt PollReceipt, detail string) (ObserverRecord, error) {
	if claim.Poll == nil || !bounded(detail, 4096) {
		return ObserverRecord{}, errors.New("invalid CI poll completion claim")
	}
	if err := validatePoll(receipt); err != nil {
		return ObserverRecord{}, err
	}
	if receipt.ID != claim.Poll.ID || receipt.OwnerEpoch != claim.Poll.OwnerEpoch || !receipt.StartedAt.Equal(claim.Poll.StartedAt) || receipt.State == "intent" {
		return ObserverRecord{}, errors.New("CI poll receipt identity differs")
	}
	if snapshot != nil {
		if snapshot.SourceDigest != claim.SourceDigest {
			return ObserverRecord{}, errors.New("CI response source differs")
		}
		if err := validateSnapshot(*snapshot); err != nil {
			return ObserverRecord{}, err
		}
	}
	tx, err := s.Store.lockObserver(ctx, claim.ID, false)
	if err != nil {
		return ObserverRecord{}, err
	}
	defer tx.close()
	r := tx.record
	if r.Driver != "direct-ci" || r.RequestDigest != claim.RequestDigest || r.SourceDigest != claim.SourceDigest || !reflect.DeepEqual(r.Owner, claim.Owner) || r.Revision < claim.Revision || r.Poll == nil || r.Poll.ID != receipt.ID || r.Poll.OwnerEpoch != receipt.OwnerEpoch || !r.Poll.StartedAt.Equal(receipt.StartedAt) {
		return ObserverRecord{}, errors.New("stale CI poll completion")
	}
	if r.Poll.State == "joined" {
		if reflect.DeepEqual(*r.Poll, receipt) {
			return cloneObserver(*r), nil
		}
		return ObserverRecord{}, errors.New("CI poll already has a different immutable join")
	}
	if r.Phase != "observing" && r.Phase != "quiescing" {
		return ObserverRecord{}, errors.New("CI observer no longer accepts results")
	}
	if r.Poll.Handle != "" && r.Poll.Handle != receipt.Handle {
		return ObserverRecord{}, errors.New("CI poll runner handle changed")
	}
	r.Poll = &receipt
	r.Detail = detail
	if snapshot != nil && receipt.State == "joined" {
		copy := *snapshot
		copy.SourceDigest = ""
		r.Remote = &copy
		if snapshot.Status == "completed" && r.Phase == "observing" && len(r.Events) == 0 {
			outcome := "failed"
			if snapshot.Conclusion == "success" {
				outcome = "passed"
			}
			op := Operation{ID: r.ID, Source: r.Source, Events: r.Events, CanceledAt: r.CanceledAt}
			if err := AddEvent(&op, ReportRequest{Slot: "terminal", Outcome: outcome, Conclusion: snapshot.Conclusion, Evidence: []string{snapshot.URL}, Detail: "Runtime observed the exact existing CI run attempt"}, s.now()); err != nil {
				return ObserverRecord{}, err
			}
			r.Events = op.Events
		}
	}
	bumpObserver(r, s.now())
	if err := tx.save(); err != nil {
		return ObserverRecord{}, err
	}
	return cloneObserver(*r), nil
}
