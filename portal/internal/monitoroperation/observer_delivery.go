package monitoroperation

import (
	"context"
	"errors"
	"os"
)

// This private event view shares delivery admission/reconciliation only. It is
// never persisted as a native operation and contains no invented native launch.
func observerEventOperation(record ObserverRecord) Operation {
	op := Operation{ID: record.ID, Workspace: record.Workspace, Owner: record.Owner, Source: record.Source, Events: record.Events, CanceledAt: record.CanceledAt}
	if record.Phase != "observing" {
		op.Disposition = record.Phase
	}
	return op
}

func (s *DeliveryService) eventOperation(ctx context.Context, id string) (Operation, error) {
	if record, err := s.Store.GetObserver(ctx, id); err == nil && record.Driver == "direct-ci" {
		return observerEventOperation(record), nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Operation{}, err
	}
	return s.Store.Get(ctx, id)
}

type eventTransaction struct {
	Operation *Operation
	save      func() error
	close     func() error
}

func (t *eventTransaction) Save() error  { return t.save() }
func (t *eventTransaction) Close() error { return t.close() }

func (s *DeliveryService) lockEventOperation(ctx context.Context, id string) (*eventTransaction, error) {
	if record, err := s.Store.GetObserver(ctx, id); err == nil && record.Driver == "direct-ci" {
		tx, err := s.Store.lockObserver(ctx, id, false)
		if err != nil {
			return nil, err
		}
		op := observerEventOperation(*tx.record)
		return &eventTransaction{Operation: &op, save: func() error {
			tx.record.Events = op.Events
			bumpObserver(tx.record, s.now())
			return tx.save()
		}, close: func() error { tx.close(); return nil }}, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	tx, err := s.Store.Lock(ctx, id)
	if err != nil {
		return nil, err
	}
	return &eventTransaction{Operation: tx.Operation, save: tx.Save, close: tx.Close}, nil
}

// Receipt reconciliation is permitted after revocation, but cannot dispatch a
// new input. Persist recovered receipts even if another receipt remains unknown.
func ReconcileObserverEvents(ctx context.Context, record *ObserverRecord, transport DeliveryTransport) error {
	if record == nil {
		return errors.New("observer receipt reconciliation is unavailable")
	}
	op := observerEventOperation(*record)
	err := ReconcileMonitorEvents(ctx, &op, transport)
	record.Events = op.Events
	return err
}

// ReconcileReceipts recovers only original event submissions. It cannot prepare
// or send a new input and remains usable while continuation is revoked.
func (s *DeliveryService) ReconcileReceipts(ctx context.Context, id string) error {
	if s.Store == nil || s.Resolve == nil || s.LockSession == nil || s.Transport == nil {
		return errors.New("monitor receipt reconciliation is unavailable")
	}
	original, err := s.eventOperation(ctx, id)
	if err != nil {
		return err
	}
	unlock, err := s.LockSession(ctx, original.Owner.Slug)
	if err != nil {
		return err
	}
	defer unlock()
	tx, err := s.lockEventOperation(ctx, id)
	if err != nil {
		return err
	}
	defer tx.Close()
	owner, err := s.Resolve(tx.Operation.Owner.Slug)
	if err != nil || !SameOwner(owner, tx.Operation.Owner) {
		return errors.New("monitor receipt recovery owner changed")
	}
	_ = ReconcileMonitorEvents(ctx, tx.Operation, s.Transport)
	return tx.Save()
}
