package inputdelivery

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
)

// Driver retains ordinary host admission through the claim and native enqueue.
// It calls claim only after fresh pre-effect checks; Lookup has no send branch.
type Driver interface {
	Dispatch(context.Context, Record, func(ExecutionEvidence) error) (Result, error)
	Lookup(context.Context, Record) (Result, error)
}
type Result struct {
	State          string
	Receipt        *backend.SendReceipt
	Failure        string
	RetiredUnknown bool
}
type PreDispatchError struct {
	Terminal bool
	Reason   string
	Err      error
}

func (e *PreDispatchError) Error() string { return e.Reason }
func (e *PreDispatchError) Unwrap() error { return e.Err }

type Dispatcher struct {
	store  *Store
	driver Driver
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	active map[string]bool
	tasks  sync.WaitGroup
}

func Start(parent context.Context, store *Store, driver Driver) *Dispatcher {
	ctx, cancel := context.WithCancel(parent)
	d := &Dispatcher{store: store, driver: driver, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}), active: map[string]bool{}}
	go d.run()
	return d
}
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
func (d *Dispatcher) Close() { d.cancel(); <-d.done; d.tasks.Wait() }
func (d *Dispatcher) run() {
	defer close(d.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	cursor := ""
	for {
		d.mu.Lock()
		slots := 4 - len(d.active)
		d.mu.Unlock()
		if slots > 0 {
			rows, err := d.store.Due(d.ctx, cursor, 4)
			if err == nil {
				for _, r := range rows {
					cursor = r.Admission.Scope
					d.mu.Lock()
					if d.active[r.Admission.Scope] || len(d.active) >= 4 {
						d.mu.Unlock()
						continue
					}
					d.active[r.Admission.Scope] = true
					d.mu.Unlock()
					d.tasks.Add(1)
					go func(r Record) {
						defer d.tasks.Done()
						defer func() { d.mu.Lock(); delete(d.active, r.Admission.Scope); d.mu.Unlock(); d.Wake() }()
						d.process(r)
					}(r)
				}
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-d.wake:
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) process(r Record) {
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	from := r.Status.State
	claimed := false
	var result Result
	var err error
	if from == "queued" {
		result, err = d.driver.Dispatch(ctx, r, func(execution ExecutionEvidence) error {
			if claimed {
				return ErrConflict
			}
			r.Status.State = "dispatching"
			r.DispatchAtMS = time.Now().UnixMilli()
			r.DispatchRuntime = execution.Runtime
			r.DispatchHandle = execution.Handle
			r.DispatchGeneration = execution.Handle.Generation
			won, e := d.store.Update(ctx, r, "queued")
			if e != nil {
				return e
			}
			if !won {
				return ErrConflict
			}
			claimed = true
			return nil
		})
		if !claimed {
			var refusal *PreDispatchError
			if errors.As(err, &refusal) && refusal.Terminal {
				r.Status.State = "not_sent"
				r.Status.Failure = refusal.Reason
				r.NextAttemptMS = 0
			} else {
				r.Status.State = "queued"
				if r.RetryDelayMS < 1000 {
					r.RetryDelayMS = 1000
				} else {
					r.RetryDelayMS *= 2
				}
				if r.RetryDelayMS > 30000 {
					r.RetryDelayMS = 30000
				}
				r.NextAttemptMS = time.Now().Add(time.Duration(r.RetryDelayMS) * time.Millisecond).UnixMilli()
			}
			// A queue write failure cannot authorize native dispatch. The next
			// scan reloads the actual durable phase before taking any action.
			_, _ = d.store.Update(d.ctx, r, "queued")
			return
		}
		from = "dispatching"
	} else {
		result, err = d.driver.Lookup(ctx, r)
	}
	r.Status.State = "unknown"
	r.Status.Failure = "Delivery could not be confirmed. Check the original delivery."
	if err == nil {
		switch result.State {
		case "sent":
			if result.Receipt != nil && result.Receipt.ClientUserMessageID == r.Status.ClientUserMessageID && result.Receipt.TurnID != "" {
				r.Status.State = "sent"
				r.Status.Receipt = result.Receipt
				r.Status.Failure = ""
			}
		case "unknown":
			if !claimed && result.RetiredUnknown {
				r.Status.Retired = true
			}
		case "pending":
			r.Status.State = "pending"
			r.Status.Failure = ""
		case "not_sent":
			r.Status.State = "not_sent"
			r.Status.Failure = result.Failure
		}
	}
	r.NextAttemptMS = nextObservation(r, time.Now().UnixMilli())
	_, _ = d.store.Update(d.ctx, r, from)
}

func nextObservation(r Record, now int64) int64 {
	if r.Status.State == "pending" && r.DispatchAtMS > 0 && now >= r.DispatchAtMS && now < r.DispatchAtMS+1000 {
		return min(now+200, r.DispatchAtMS+1000)
	}
	return now + 2000
}
