package main

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Utilities are owned execution, never cooperative maintenance participants.
// Their original reports, receipt uncertainty and native effects retain the
// existing monitor proof contract, including ordinary lazy submission ledger
// initialization/import/WAL/fence bookkeeping. It cannot launch,
// resume, cancel, archive or manufacture a continuation for a utility.
func maintenanceMonitorInventory(ctx context.Context, store *monitoroperation.Store, summary session.Summary, socket string, client monitoroperation.LifecycleClient) (monitoroperation.Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	finish := monitoroperation.BeginProof(ctx, "", "inventory")
	view, err := store.Inventory(ctx, summary.Slug, summary.Codex.ThreadID)
	finish(err, false)
	if err != nil {
		return view, &monitoroperation.DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	finish = monitoroperation.BeginProof(ctx, "", "inventory-list")
	operations, err := store.List(ctx, summary.Slug)
	finish(err, false)
	if err != nil {
		return view, &monitoroperation.DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	retained := false
	for _, op := range operations {
		retained = retained || op.Disposition != "archived"
	}
	finish = monitoroperation.BeginProof(ctx, "", "inventory-observers")
	records, err := store.ListObservers(ctx, summary.Slug)
	finish(err, false)
	if err != nil {
		return view, &monitoroperation.DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	if !retained && len(records) == 0 {
		return view, nil
	}
	finish = monitoroperation.BeginProof(ctx, "", "owner-identity")
	owner, err := monitoroperation.IdentityFor(summary.Workspace, summary.Slug)
	finish(err, false)
	if err != nil {
		return view, &monitoroperation.DiagnosticError{Stage: "owner-identity", Category: "unavailable-proof", Err: err}
	}
	owner.Socket = socket
	finish = monitoroperation.BeginProof(ctx, "", "owner-match")
	recordedOwner := ""
	for _, op := range operations {
		if op.Disposition != "archived" && !monitoroperation.SameOwner(op.Owner, owner) {
			finish(nil, true)
			return view, &monitoroperation.DiagnosticError{Stage: "owner-match", Category: "identity-mismatch", MonitorID: op.ID, OwnerSlug: op.Owner.Slug, Err: errors.New("monitor maintenance owner changed")}
		}
		if len(view.Unresolved) != 0 && op.ID == view.Unresolved[0] {
			recordedOwner = op.Owner.Slug
		}
	}
	for _, record := range records {
		if !monitoroperation.SameOwner(record.Owner, owner) {
			finish(nil, true)
			return view, &monitoroperation.DiagnosticError{Stage: "owner-match", Category: "identity-mismatch", MonitorID: record.ID, OwnerSlug: record.Owner.Slug, Err: errors.New("monitor maintenance observer owner changed")}
		}
		if len(view.Unresolved) != 0 && record.ID == view.Unresolved[0] {
			recordedOwner = record.Owner.Slug
		}
	}
	finish(nil, false)
	if len(view.Unresolved) != 0 {
		return view, &monitoroperation.DiagnosticError{Stage: "receipt-proof", Category: "uncertain-state", MonitorID: view.Unresolved[0], OwnerSlug: recordedOwner, Err: errors.New("monitor operation retains uncertain execution or delivery")}
	}
	// Inventory's cached termination marker is not native/effects proof. Reuse
	// the lifecycle proof even for a saved terminal report or an idle wrapper.
	finish = monitoroperation.BeginProof(ctx, "", "local-safety")
	local, err := (monitoroperation.Lifecycle{Store: store, Client: client}).AssessLocalSafety(ctx, summary.Slug, summary.Codex.ThreadID)
	if err == nil && local.State == "blocked" {
		err = errors.New(local.Reason)
		if local.Diagnostic != nil {
			detail := *local.Diagnostic
			detail.Err, detail.Cause = err, local.Diagnostic
			err = &detail
		}
	}
	finish(err, false)
	return view, err
}

var maintenanceMonitorUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func maintenanceMonitorReason(prefix string, err error) string {
	stage, category, id, slug := "unknown", "unavailable-proof", "unknown", "unknown"
	var detail *monitoroperation.DiagnosticError
	if errors.As(err, &detail) {
		switch detail.Stage {
		case "inventory-store", "owner-identity", "owner-match", "receipt-proof", "native-project", "native-thread", "native-bootstrap", "native-archive-state", "native-idle":
			stage = detail.Stage
		}
		switch detail.Category {
		case "uncertain-state", "identity-mismatch", "unavailable-proof":
			category = detail.Category
		}
		if maintenanceMonitorUUID.MatchString(detail.MonitorID) {
			id = detail.MonitorID
		}
		if len(detail.OwnerSlug) <= 128 && session.ValidSlug(detail.OwnerSlug) {
			slug = detail.OwnerSlug
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		category = "deadline"
	} else if errors.Is(err, context.Canceled) {
		category = "cancelled"
	}
	suffix := ";monitor-stage=" + stage + ";monitor-category=" + category + ";monitor-id=" + id + ";owner-slug=" + slug
	// The prefix contains only the three fixed aggregate categories. Never cut an identity.
	if len(prefix)+len(suffix) > 1024 {
		return prefix + ";monitor-stage=unknown;monitor-category=unavailable-proof;monitor-id=unknown;owner-slug=unknown"
	}
	return prefix + suffix
}

func maintenanceMonitorBlockers(blockers []string, first error) error {
	refusal := maintenanceInventoryBlockers(blockers)
	if first == nil {
		return refusal
	}
	var proof *maintenance.ProofError
	if !errors.As(refusal, &proof) {
		return refusal
	}
	var detail *monitoroperation.DiagnosticError
	if !errors.As(first, &detail) {
		detail = &monitoroperation.DiagnosticError{}
	}
	result := *detail
	result.Err = &maintenance.ProofError{Reason: maintenanceMonitorReason(proof.Reason, first)}
	result.Cause = first
	return &result
}
