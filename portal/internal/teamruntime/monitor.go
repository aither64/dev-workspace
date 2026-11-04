package teamruntime

import (
	"context"
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

func (service Service) monitorLifecycle() (monitoroperation.Lifecycle, error) {
	store, err := monitoroperation.NewStore(service.Store.stateRoot, service.Store.workspace)
	return monitoroperation.Lifecycle{Store: store, Client: service.Client}, err
}

func (service Service) requireMonitorIdle(ctx context.Context, slug, root string, executionOnly bool) error {
	lifecycle, err := service.monitorLifecycle()
	if err != nil {
		return err
	}
	return lifecycle.RequireIdle(ctx, slug, root, executionOnly)
}

func (service Service) includeMonitorActive(ctx context.Context, slug, root string, active map[string]string) error {
	lifecycle, err := service.monitorLifecycle()
	if err != nil {
		return err
	}
	inventory, err := lifecycle.Store.Inventory(ctx, slug, root)
	if err != nil {
		return err
	}
	if len(inventory.Unresolved) != 0 {
		return errors.New("monitor ownership has an unfinished launch, submission or retirement")
	}
	for _, thread := range inventory.Threads {
		op, err := lifecycle.Store.Get(ctx, thread.OperationID)
		if err != nil {
			return err
		}
		state, err := lifecycle.State(ctx, op)
		if err != nil {
			return err
		}
		if state != workspacecodex.ArchiveArchived {
			if _, exists := active[thread.ThreadID]; exists {
				return errors.New("monitor thread duplicates retained conversation ownership")
			}
			active[thread.ThreadID] = thread.ProjectID
		}
	}
	return nil
}
