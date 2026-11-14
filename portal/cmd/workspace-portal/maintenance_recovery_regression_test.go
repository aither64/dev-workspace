package main

import (
	"context"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceCancellationRepairIsLimitedToUnchangedSource(t *testing.T) {
	for _, scenario := range []string{"accepted", "not-cancelled", "cutover", "stopped", "wrong-owner", "activation-action", "changed-profile"} {
		t.Run(scenario, func(t *testing.T) {
			m, o := maintenanceWorkerFixture(t, false)
			source := filepath.Join(m.store.Root, "source")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(source, m.store.Profile); err != nil {
				t.Fatal(err)
			}
			_, err := m.store.Update(context.Background(), o.OperationID, 0, func(saved *maintenance.Operation) error {
				saved.SourcePackage = source
				saved.CancelRequested = scenario != "not-cancelled"
				saved.CutoverIntent = scenario == "cutover"
				saved.SourcesStopped = scenario == "stopped"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			action, owner := "continue", source
			if scenario == "activation-action" {
				action = "activation-intent"
			}
			if scenario == "wrong-owner" {
				owner = m.store.Root
			}
			if scenario == "changed-profile" {
				if err := os.Remove(m.store.Profile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(m.store.Root, m.store.Profile); err != nil {
					t.Fatal(err)
				}
			}
			err = maintenanceCancellationRepair(m.store, maintenance.Request{Action: action, OperationID: o.OperationID}, owner)
			if (err == nil) != (scenario == "accepted") {
				t.Fatal("incorrect cancellation repair admission", err)
			}
		})
	}
}
