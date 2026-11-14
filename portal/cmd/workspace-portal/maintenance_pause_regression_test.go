package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
)

func TestMaintenanceLargeSettledChildInventoryFitsPauseAllowance(t *testing.T) {
	m, o, _ := maintenancePauseFixture(t, true)
	m.pauseRecoveryTimeout = 3 * time.Second
	_, err := m.store.Update(context.Background(), o.OperationID, 0, func(saved *maintenance.Operation) error {
		saved.PauseDeadline = time.Now().Add(-time.Second)
		for i := range 1222 {
			saved.Children = append(saved.Children, maintenance.Child{Workspace: m.workspace, Parent: "thread", Thread: fmt.Sprintf("historic-%d", i)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := m.pause(ctx, o.OperationID)
	if err != nil || result.Status != "paused" {
		t.Fatalf("large retained inventory exhausted bounded pause: %+v %v", result, err)
	}
}

func TestMaintenancePauseChecksObserveCancellationAndInvalidReplacement(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		m, o := maintenanceWorkerFixture(t, false)
		m.pauseState = &maintenancePauseState{}
		ctx := context.Background()
		if err := m.checkPause(ctx, o.OperationID); err != nil {
			t.Fatal(err)
		}
		if invalid {
			path := m.store.Path(o.OperationID)
			if err := os.WriteFile(path+".replacement", []byte(`{"schema":2,"cancelRequested":false}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".replacement", path); err != nil {
				t.Fatal(err)
			}
			if err := m.checkPause(ctx, o.OperationID); err == nil {
				t.Fatal("cached permission survived an invalid journal replacement")
			}
		} else {
			_, err := m.store.Update(ctx, o.OperationID, 0, func(saved *maintenance.Operation) error { saved.CancelRequested = true; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := m.checkPause(ctx, o.OperationID); !errors.Is(err, errMaintenancePauseCancelled) {
				t.Fatal("cached permission concealed cancellation", err)
			}
		}
	}
}
