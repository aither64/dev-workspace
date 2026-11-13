package monitoroperation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

func (l Lifecycle) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func observerReceiptsKnown(record ObserverRecord) error {
	return knownMonitorReceipts(Operation{Events: record.Events})
}

// A terminal event permanently fences future ClaimCIPoll calls. Together with
// its validated completed snapshot and exact joined poll, this permits ordinary
// retirement through revoke without manufacturing accepted archive authority.
func completedDirectObserver(record ObserverRecord) bool {
	if record.Driver != "direct-ci" || record.Phase != "observing" || record.Remote == nil ||
		record.Remote.Status != "completed" || validateSnapshot(*record.Remote) != nil ||
		record.Poll == nil || record.Poll.State != "joined" || validatePoll(*record.Poll) != nil || len(record.Events) != 1 {
		return false
	}
	report := record.Events[0].Report
	want := "failed"
	if record.Remote.Conclusion == "success" {
		want = "passed"
	}
	return report.Slot == "terminal" && report.Outcome == want && report.Conclusion == record.Remote.Conclusion
}

func (l Lifecycle) requireObserverIdle(ctx context.Context, slug, root string, executionOnly bool) error {
	records, err := l.Store.ListObservers(ctx, slug)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Owner.RootThreadID != root {
			return errors.New("observer lifecycle owner changed")
		}
		if record.Driver != "direct-ci" {
			continue
		}
		if err := observerReceiptsKnown(record); err != nil {
			return err
		}
		if record.Poll != nil && record.Poll.State != "joined" {
			return errors.New("observer local polling effects remain unproved")
		}
		if !executionOnly && record.Phase != "detached" && !completedDirectObserver(record) {
			return errors.New("observer must be detached by its accepted archive owner")
		}
	}
	return nil
}

// These callers already hold ordinary lifecycle admission. Revocation changes
// continuation only; it cannot join an unknown poll or cancel the remote run.
func (l Lifecycle) revokeDirectObservers(ctx context.Context, slug, root string) error {
	records, err := l.Store.ListObservers(ctx, slug)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Owner.RootThreadID != root {
			return errors.New("observer revocation owner changed")
		}
		if record.Driver != "direct-ci" {
			continue
		}
		tx, err := l.Store.lockObserver(ctx, record.ID, false)
		if err != nil {
			return err
		}
		err = func() error {
			r := tx.record
			if r.CanceledAt == nil {
				now := l.now()
				r.CanceledAt = &now
			}
			cancelObserverEvents(r)
			bumpObserver(r, l.now())
			return tx.save()
		}()
		tx.close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (l Lifecycle) requireObserverRetired(ctx context.Context, slug, root string) error {
	records, err := l.Store.ListObservers(ctx, slug)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Owner.RootThreadID != root {
			return errors.New("observer retirement owner changed")
		}
		if record.Driver == "direct-ci" && record.Phase != "detached" && (!completedDirectObserver(record) || record.CanceledAt == nil) {
			return errors.New("observer continuation retirement is unfinished")
		}
	}
	return nil
}

// AssessLocalSafety never calls Poll/Quiesce or creates a record. Native reads
// remain cold and preserve all original receipt/utility proof requirements.
func (l Lifecycle) AssessLocalSafety(ctx context.Context, slug, root string) (LocalSafety, error) {
	view := LocalSafety{State: "quiescent", OperationIDs: []string{}}
	var blockedIDs, attachedIDs []string
	block := func(id, reason string) {
		if len(blockedIDs) == 0 {
			view.Reason = reason
		}
		blockedIDs = append(blockedIDs, id)
	}
	operations, err := l.Store.List(ctx, slug)
	if err != nil {
		return view, err
	}
	for _, op := range operations {
		if op.Owner.RootThreadID != root {
			if op.Disposition == "archived" {
				continue
			}
			block(op.ID, "monitor retained owner changed")
			continue
		}
		if HasUnresolvedEffects(op) {
			block(op.ID, "native monitor local effects require original resolution")
			continue
		}
		if err := l.ProveQuiescent(ctx, op); err != nil {
			block(op.ID, err.Error())
		}
	}
	records, err := l.Store.ListObservers(ctx, slug)
	if err != nil {
		return view, err
	}
	for _, record := range records {
		if record.Owner.RootThreadID != root {
			block(record.ID, "observer retained owner changed")
			continue
		}
		if record.Driver != "direct-ci" {
			continue
		}
		if err := observerReceiptsKnown(record); err != nil {
			block(record.ID, err.Error())
			continue
		}
		if record.Phase == "detached" || completedDirectObserver(record) {
			continue
		}
		if record.Poll != nil && record.Poll.State != "joined" {
			known, ok := l.Observer.(interface {
				OwnsPoll(string, PollReceipt) bool
			})
			if !ok || !known.OwnsPoll(record.ID, *record.Poll) {
				block(record.ID, "observer poll ownership or completion is unknown")
				continue
			}
		}
		attachedIDs = append(attachedIDs, record.ID)
	}
	if len(blockedIDs) != 0 {
		view.State, view.OperationIDs = "blocked", blockedIDs
	} else if len(attachedIDs) != 0 {
		view.State, view.OperationIDs = "quiesceable", attachedIDs
	}
	return view, nil
}

func (l Lifecycle) authorizeObserverArchive(ctx context.Context, a ArchiveAuthority) error {
	if err := validateArchive(a); err != nil {
		return err
	}
	if l.AuthorizeArchive == nil {
		return errors.New("observer shutdown requires validated ordinary archive admission")
	}
	return l.AuthorizeArchive(ctx, a)
}

func (l Lifecycle) BeginArchiveQuiescence(ctx context.Context, a ArchiveAuthority) error {
	if err := l.authorizeObserverArchive(ctx, a); err != nil {
		return err
	}
	records, err := l.Store.ListObservers(ctx, a.Slug)
	if err != nil {
		return err
	}
	for _, record := range records {
		tx, err := l.Store.lockObserver(ctx, record.ID, false)
		if err != nil {
			return err
		}
		err = func() error {
			r := tx.record
			if r.Owner.RootThreadID != a.RootThreadID {
				return errors.New("observer archive owner changed")
			}
			if r.Archive != nil && !reflect.DeepEqual(*r.Archive, a) {
				return errors.New("observer belongs to a different accepted archive")
			}
			if r.Archive != nil {
				return nil
			}
			copy := a
			r.Archive = &copy
			r.Phase = "quiescing"
			now := l.now()
			if r.CanceledAt == nil {
				r.CanceledAt = &now
			}
			cancelObserverEvents(r)
			bumpObserver(r, now)
			return tx.save()
		}()
		tx.close()
		if err != nil {
			return err
		}
	}
	// Revocation preserves accepted/unknown receipts and does not stop utilities.
	return l.Revoke(ctx, a.Slug, a.RootThreadID)
}

func (l Lifecycle) CompleteArchiveQuiescence(ctx context.Context, a ArchiveAuthority) (LocalSafety, error) {
	view := LocalSafety{State: "quiescent", OperationIDs: []string{}}
	if err := l.authorizeObserverArchive(ctx, a); err != nil {
		return view, err
	}
	records, err := l.Store.ListObservers(ctx, a.Slug)
	if err != nil {
		return view, err
	}
	for _, record := range records {
		if record.Owner.RootThreadID != a.RootThreadID {
			return view, errors.New("observer archive owner changed")
		}
		if record.Archive == nil || !reflect.DeepEqual(*record.Archive, a) {
			return view, errors.New("observer quiescence intent is not this archive's")
		}
		if record.Phase == "detached" {
			continue
		}
		var joined *PollReceipt
		var joinErr error
		if record.Poll != nil && record.Poll.State != "joined" {
			if l.Observer == nil {
				joinErr = errors.New("execution owner must publish the exact joined observer receipt")
			} else {
				receipt, err := l.Observer.Quiesce(ctx, record.ID)
				joinErr = err
				if err == nil {
					if err = validatePoll(receipt); err == nil && receipt.State == "joined" {
						joined = &receipt
					} else {
						joinErr = errors.New("observer shutdown has no positive joined receipt")
					}
				}
			}
		}
		// Never hold the per-ID lock while waiting for the execution owner.
		tx, err := l.Store.lockObserver(ctx, record.ID, false)
		if err != nil {
			return view, err
		}
		err = func() error {
			r := tx.record
			if r.Phase == "detached" {
				return nil
			}
			if r.Archive == nil || !reflect.DeepEqual(*r.Archive, a) {
				return errors.New("observer archive changed while joining")
			}
			if r.Poll != nil && r.Poll.State != "joined" {
				if joined == nil {
					return errors.Join(joinErr, errors.New("observer local effects remain uncertain"))
				}
				p := r.Poll
				if joined.ID != p.ID || joined.OwnerEpoch != p.OwnerEpoch || !joined.StartedAt.Equal(p.StartedAt) || p.Handle != "" && joined.Handle != p.Handle {
					return errors.New("observer join receipt belongs to another call")
				}
				r.Poll = joined
				bumpObserver(r, l.now())
				if err := tx.save(); err != nil {
					return err
				}
			}
			if err := observerReceiptsKnown(*r); err != nil {
				return err
			}
			if r.LocalProof == nil {
				evidence := []string{"No poll intent was admitted"}
				if r.Poll != nil {
					evidence = []string{"poll:" + r.Poll.ID, "receipt-sha256:" + digest(r.Poll)}
				}
				r.LocalProof = &LocalProof{Kind: "runtime-joined", At: l.now(), Evidence: evidence, Detail: "All admitted read-only CI polling requests are positively joined or unsent"}
			}
			r.Phase = "detached"
			cancelObserverEvents(r)
			bumpObserver(r, l.now())
			return tx.save()
		}()
		tx.close()
		if err != nil {
			view.State = "blocked"
			view.Reason = fmt.Sprintf("monitor %s: %v", record.ID, err)
			view.OperationIDs = append(view.OperationIDs, record.ID)
			return view, nil
		}
	}
	// Native utilities retain their original effects and quiescence requirements.
	return l.AssessLocalSafety(ctx, a.Slug, a.RootThreadID)
}
