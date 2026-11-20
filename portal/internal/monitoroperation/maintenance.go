package monitoroperation

import (
	"context"
	"errors"
)

// MaintenanceInventory reuses one validated list pair within this proof only.
// Lists still validate the entire workspace before filtering; no cache survives
// return. Only projections and owner checks intervene before native assessment,
// whose late observer scan remains fresh. The caller owns the original budget.
func (l Lifecycle) MaintenanceInventory(ctx context.Context, slug, root, socket string) (Inventory, error) {
	operations, err := l.Store.List(ctx, slug)
	if err != nil {
		return Inventory{}, &DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	view, err := l.Store.inventoryOperations(operations, slug, root)
	if err != nil {
		return view, &DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	records, err := l.Store.ListObservers(ctx, slug)
	if err != nil {
		return view, &DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	view, err = inventoryObservers(view, records, root)
	if err != nil {
		return view, &DiagnosticError{Stage: "inventory-store", Category: "unavailable-proof", Err: err}
	}
	retained := false
	for _, op := range operations {
		retained = retained || op.Disposition != "archived"
	}
	if !retained && len(records) == 0 {
		return view, nil
	}
	owner, err := retainedIdentityFor(l.Store.workspace, slug)
	if err != nil {
		return view, &DiagnosticError{Stage: "owner-identity", Category: "unavailable-proof", Err: err}
	}
	owner.Socket = socket
	recordedOwner := ""
	for _, op := range operations {
		if op.Disposition != "archived" && !SameOwner(op.Owner, owner) {
			return view, &DiagnosticError{Stage: "owner-match", Category: "identity-mismatch", MonitorID: op.ID, OwnerSlug: op.Owner.Slug, Err: errors.New("monitor maintenance owner changed")}
		}
		if len(view.Unresolved) != 0 && op.ID == view.Unresolved[0] {
			recordedOwner = op.Owner.Slug
		}
	}
	for _, record := range records {
		if !SameOwner(record.Owner, owner) {
			return view, &DiagnosticError{Stage: "owner-match", Category: "identity-mismatch", MonitorID: record.ID, OwnerSlug: record.Owner.Slug, Err: errors.New("monitor maintenance observer owner changed")}
		}
		if len(view.Unresolved) != 0 && record.ID == view.Unresolved[0] {
			recordedOwner = record.Owner.Slug
		}
	}
	if len(view.Unresolved) != 0 {
		return view, &DiagnosticError{Stage: "receipt-proof", Category: "uncertain-state", MonitorID: view.Unresolved[0], OwnerSlug: recordedOwner, Err: errors.New("monitor operation retains uncertain execution or delivery")}
	}
	// Inventory's cached termination marker is not native/effects proof. Reuse
	// the lifecycle proof even for a saved terminal report or an idle wrapper.
	local, err := l.assessLocalSafety(ctx, slug, root, operations)
	if err == nil && local.State == "blocked" {
		err = errors.New(local.Reason)
		if local.Diagnostic != nil {
			detail := *local.Diagnostic
			detail.Err, detail.Cause = err, local.Diagnostic
			err = &detail
		}
	}
	return view, err
}
