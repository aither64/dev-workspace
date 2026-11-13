package monitoroperation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aither64/codex-web/codex"
)

// DeliveryTransport never grants execution permission. Ready must prove current
// session admission before Send can cross the configured client's cold boundary.
type DeliveryTransport interface {
	Ready(context.Context, Owner) (string, error)
	Prepare(context.Context, Owner) (codex.TurnOptions, error)
	Send(context.Context, Owner, Event) (codex.SendReceipt, error)
	Reconcile(context.Context, Owner, Event) (codex.SendReceipt, bool, error)
}

type UtilityObserver interface {
	ObserveUtility(context.Context, Operation) (termination string, terminal bool, err error)
}

type DeliveryService struct {
	Store       *Store
	Resolve     func(string) (Owner, error)
	LockSession func(context.Context, string) (func(), error)
	Transport   DeliveryTransport
	Observer    UtilityObserver
	Now         func() time.Time
}

func (s *DeliveryService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *DeliveryService) Pending(ctx context.Context) ([]EventRef, error) {
	if s.Store == nil {
		return nil, errors.New("monitor delivery store is unavailable")
	}
	ops, err := s.Store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	var refs []EventRef
	for _, op := range ops {
		for _, event := range op.Events {
			switch event.Delivery.Status {
			case "pending", "held", "submitting":
				refs = append(refs, EventRef{OperationID: op.ID, EventID: event.ID})
			}
		}
	}
	observers, err := s.Store.ListObservers(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, observer := range observers {
		if observer.Driver != "direct-ci" {
			continue
		}
		for _, event := range observer.Events {
			switch event.Delivery.Status {
			case "pending", "held", "submitting":
				refs = append(refs, EventRef{OperationID: observer.ID, EventID: event.ID})
			}
		}
	}
	return refs, nil
}

// Dispatch takes runtime admission before the operation transaction. An unknown
// native outcome is reconciled even after revocation, but can never authorize a
// new send without current ownership and readiness.
func (s *DeliveryService) Dispatch(ctx context.Context, operationID, eventID string) error {
	if s.Store == nil || s.Resolve == nil || s.LockSession == nil || s.Transport == nil {
		return errors.New("monitor delivery is unavailable")
	}
	original, err := s.eventOperation(ctx, operationID)
	if err != nil {
		return err
	}
	unlock, err := s.LockSession(ctx, original.Owner.Slug)
	if err != nil {
		return err
	}
	defer unlock()
	tx, err := s.lockEventOperation(ctx, operationID)
	if err != nil {
		return err
	}
	defer tx.Close()
	op := tx.Operation
	var event *Event
	for i := range op.Events {
		if op.Events[i].ID == eventID {
			event = &op.Events[i]
			break
		}
	}
	if event == nil {
		return errors.New("monitor event does not exist")
	}
	delivery := &event.Delivery
	if delivery.Status == "accepted" || delivery.Status == "canceled" {
		return nil
	}
	if delivery.Status == "submitting" {
		receipt, found, err := s.Transport.Reconcile(ctx, op.Owner, *event)
		if err != nil {
			delivery.Detail = "Delivery is unconfirmed; the original receipt will be reconciled"
			return tx.Save()
		}
		if found {
			confirmMonitorDelivery(delivery, receipt)
			return tx.Save()
		}
	}
	if op.CanceledAt != nil || op.Disposition != "" {
		delivery.Status, delivery.Detail = "canceled", "Operation continuation was revoked"
		return tx.Save()
	}
	owner, err := s.Resolve(op.Owner.Slug)
	if err != nil {
		delivery.Status, delivery.Detail = "held", "Session ownership cannot be verified"
		return tx.Save()
	}
	if !SameOwner(owner, op.Owner) {
		delivery.Status, delivery.Detail = "canceled", "Session ownership changed"
		return tx.Save()
	}
	reason, err := s.Transport.Ready(ctx, owner)
	if err != nil {
		reason = "Session activity is unavailable"
	}
	if reason != "" {
		delivery.Status, delivery.Detail = "held", reason
		return tx.Save()
	}
	if delivery.Options == nil {
		options, err := s.Transport.Prepare(ctx, owner)
		if err != nil {
			delivery.Status, delivery.Detail = "held", "Lead settings are unavailable"
			return tx.Save()
		}
		delivery.Options = &options
	}
	delivery.Status, delivery.Detail = "submitting", ""
	delivery.AdmittedEpoch = owner.SocketEpoch
	if err := tx.Save(); err != nil {
		return err
	}
	receipt, err := s.Transport.Send(ctx, owner, *event)
	if err != nil {
		var unknown *codex.UnknownSendOutcomeError
		var unsent *codex.MessageNotSentError
		if errors.As(err, &unknown) {
			delivery.Detail = "Delivery is unconfirmed; the original receipt will be reconciled"
		} else if errors.As(err, &unsent) {
			delivery.Status = "held"
			delivery.Detail = "Delivery was not sent; the original request is retained"
		} else {
			delivery.Detail = "Delivery is unconfirmed; the original receipt will be reconciled"
		}
	} else {
		confirmMonitorDelivery(delivery, receipt)
	}
	return tx.Save()
}

func confirmMonitorDelivery(delivery *Delivery, receipt codex.SendReceipt) {
	delivery.Receipt = &receipt
	if receipt.TurnID == "" {
		// Native queue persistence is not evidence that a lead turn started.
		delivery.Status = "submitting"
		delivery.Detail = "Input is retained without confirmed lead turn activation"
		return
	}
	delivery.Status, delivery.Detail = "accepted", ""
}

// ReconcileMonitorEvents is read/reconcile-only recovery for status and evidence
// resolution. The caller holds session admission and the operation transaction,
// and must save any recovered proofs even when another receipt remains unknown.
// It never calls Ready, Prepare or Send and never changes an input identity.
func ReconcileMonitorEvents(ctx context.Context, op *Operation, transport DeliveryTransport) error {
	if op == nil || transport == nil {
		return errors.New("monitor receipt reconciliation is unavailable")
	}
	var unresolved error
	for i := range op.Events {
		event := &op.Events[i]
		if event.Delivery.Status != "submitting" {
			continue
		}
		receipt, found, err := transport.Reconcile(ctx, op.Owner, *event)
		if err != nil {
			event.Delivery.Detail = "Delivery is unconfirmed; the original receipt will be reconciled"
			unresolved = errors.New("monitor input receipt remains unconfirmed")
			continue
		}
		if found {
			confirmMonitorDelivery(&event.Delivery, receipt)
			continue
		}
		// The transport returns no error and no receipt only for a definitely
		// unsubmitted (absent/prepared) attempt. Missing history stays an error.
		if op.CanceledAt != nil || op.Disposition != "" {
			event.Delivery.Status, event.Delivery.Detail = "canceled", "Operation continuation was revoked"
		} else {
			event.Delivery.Status, event.Delivery.Detail = "held", "The original input has not been submitted"
		}
	}
	return unresolved
}

// Observe never resumes a utility. Only an exact native terminal observation
// releases live-watcher capacity, regardless of whether a report already exists.
func (s *DeliveryService) Observe(ctx context.Context, operationID string) error {
	if s.Store == nil || s.LockSession == nil || s.Observer == nil {
		return errors.New("monitor observation is unavailable")
	}
	op, err := s.Store.Get(ctx, operationID)
	if err != nil {
		return err
	}
	if op.Launch.TerminatedAt != nil || op.Launch.Receipt == nil || op.Launch.Receipt.TurnID == "" {
		return nil
	}
	unlock, err := s.LockSession(ctx, op.Owner.Slug)
	if err != nil {
		return err
	}
	defer unlock()
	tx, err := s.Store.Lock(ctx, operationID)
	if err != nil {
		return err
	}
	defer tx.Close()
	op = *tx.Operation
	if op.Launch.TerminatedAt != nil {
		return nil
	}
	termination, terminal, err := s.Observer.ObserveUtility(ctx, op)
	if err != nil || !terminal {
		return err
	}
	at := s.now()
	tx.Operation.Launch.TerminatedAt, tx.Operation.Launch.Termination = &at, termination
	for _, event := range op.Events {
		if event.Report.Slot == "terminal" {
			return tx.Save()
		}
	}
	if err := AddEvent(tx.Operation, ReportRequest{Slot: "terminal", Outcome: "incomplete",
		Detail: fmt.Sprintf("The utility turn %s ended (%s) without a terminal report. Command completion is unknown; inspect the operation evidence before taking further action.", op.Launch.Receipt.TurnID, termination)}, at); err != nil {
		return err
	}
	return tx.Save()
}
