package main

import (
	"context"
	"errors"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Utilities are owned execution, never cooperative maintenance participants.
// Their original reports, receipt uncertainty and native effects retain the
// existing monitor proof contract. This path only reads; it cannot launch,
// resume, cancel, archive or manufacture a continuation for a utility.
func maintenanceMonitorInventory(ctx context.Context, store *monitoroperation.Store, summary session.Summary, socket string, client monitoroperation.LifecycleClient) (monitoroperation.Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	view, err := store.Inventory(ctx, summary.Slug, summary.Codex.ThreadID)
	if err != nil {
		return view, err
	}
	operations, err := store.List(ctx, summary.Slug)
	if err != nil {
		return view, err
	}
	retained := false
	for _, op := range operations {
		retained = retained || op.Disposition != "archived"
	}
	records, err := store.ListObservers(ctx, summary.Slug)
	if err != nil {
		return view, err
	}
	if !retained && len(records) == 0 {
		return view, nil
	}
	owner, err := monitoroperation.IdentityFor(summary.Workspace, summary.Slug)
	if err != nil {
		return view, err
	}
	owner.Socket = socket
	for _, op := range operations {
		if op.Disposition != "archived" && !monitoroperation.SameOwner(op.Owner, owner) {
			return view, errors.New("monitor maintenance owner changed")
		}
	}
	for _, record := range records {
		if !monitoroperation.SameOwner(record.Owner, owner) {
			return view, errors.New("monitor maintenance observer owner changed")
		}
	}
	if len(view.Unresolved) != 0 {
		return view, errors.New("monitor operation retains uncertain execution or delivery")
	}
	// Inventory's cached termination marker is not native/effects proof. Reuse
	// the lifecycle proof even for a saved terminal report or an idle wrapper.
	local, err := (monitoroperation.Lifecycle{Store: store, Client: client}).AssessLocalSafety(ctx, summary.Slug, summary.Codex.ThreadID)
	if err == nil && local.State == "blocked" {
		err = errors.New(local.Reason)
	}
	return view, err
}
