package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
)

// The host owns cancellation admission and the final preparation fence. A
// preparation helper observes that intent even while a native request or a
// runtime lock is waiting. This never interrupts the shared native daemon.
func providerPreparationContext(parent context.Context, snapshot, operation string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	check := func() {
		path := filepath.Join(filepath.Dir(snapshot), "state.json")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			// Legacy callers and isolated helper fixtures have no scoped state.
			return
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			cancel(errors.New("provider cancellation state is unavailable"))
			return
		}
		data, err := os.ReadFile(path)
		var state struct {
			Operation struct {
				ID       string `json:"id"`
				Strategy string `json:"strategy"`
				Phase    string `json:"phase"`
			} `json:"operation"`
		}
		if err != nil || json.Unmarshal(data, &state) != nil || state.Operation.ID != operation {
			cancel(errors.New("provider preparation operation changed"))
		} else if state.Operation.Strategy == "scoped-v1" {
			control, err := codexproviders.ReadOperationControl(filepath.Dir(snapshot), operation)
			if err != nil {
				cancel(err)
			} else if control.CancelRequested || state.Operation.Phase == "canceled" {
				cancel(errors.New("provider preparation canceled"))
			}
		}
	}
	check()
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
	return ctx, func() { cancel(context.Canceled) }
}
