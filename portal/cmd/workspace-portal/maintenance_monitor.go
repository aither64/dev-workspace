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
// existing monitor proof contract, including ordinary submission ledger bookkeeping.
// It cannot launch,
// resume, cancel, archive or manufacture a continuation for a utility.
func maintenanceMonitorInventory(ctx context.Context, store *monitoroperation.Store, summary session.Summary, socket string, client monitoroperation.LifecycleClient) (monitoroperation.Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return (monitoroperation.Lifecycle{Store: store, Client: client}).MaintenanceInventory(ctx, summary.Slug, summary.Codex.ThreadID, socket)
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
