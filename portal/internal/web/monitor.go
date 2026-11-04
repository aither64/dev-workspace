package web

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type monitorDeliveryClient struct {
	monitoroperation.DeliveryClient
	activity *activityMonitor
}

func (c monitorDeliveryClient) ReadActivity(ctx context.Context, thread string) (codex.ActivitySnapshot, error) {
	if c.activity != nil {
		return c.activity.readFreshActivity(ctx, thread)
	}
	return c.DeliveryClient.ReadActivity(ctx, thread)
}

func (s *Server) monitorDeliveryService() (*monitoroperation.DeliveryService, error) {
	client, ok := s.config.Codex.(monitoroperation.DeliveryClient)
	if !ok || s.config.UserStateRoot == "" {
		return nil, errors.New("monitor delivery is unavailable")
	}
	store, err := monitoroperation.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		return nil, err
	}
	native := monitoroperation.NativeDelivery{Workspace: s.config.Workspace, StateRoot: s.config.UserStateRoot,
		AuthorityDir: s.config.AuthorityDir, Socket: s.config.CodexSocket,
		Client:      monitorDeliveryClient{DeliveryClient: client, activity: s.activity},
		Admission:   func(owner monitoroperation.Owner) error { return s.requireProviderAdmission(owner.Slug) },
		PrepareLead: s.prepareMonitorLead,
	}
	return &monitoroperation.DeliveryService{Store: store, Transport: native, Observer: native,
		Resolve: func(slug string) (monitoroperation.Owner, error) {
			return monitoroperation.ResolveDeliveryOwner(s.config.Workspace, slug)
		},
		LockSession: func(ctx context.Context, slug string) (func(), error) {
			runtime, err := session.LockRuntimeSharedContext(ctx, s.config.AuthorityDir, slug)
			if err != nil {
				return nil, err
			}
			team, err := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
			if err != nil {
				runtime.Close()
				return nil, err
			}
			operation, err := team.LockOperation(ctx, slug)
			if err != nil {
				runtime.Close()
				return nil, err
			}
			return func() { operation(); _ = runtime.Close() }, nil
		},
	}, nil
}

func (s *Server) prepareMonitorLead(ctx context.Context, owner monitoroperation.Owner) (codex.TurnOptions, error) {
	client, ok := s.config.Codex.(interface {
		ReadThreadSettings(context.Context, string) (codex.ThreadSettings, error)
	})
	if !ok {
		return codex.TurnOptions{}, errors.New("lead settings are unavailable")
	}
	settings, err := client.ReadThreadSettings(ctx, owner.RootThreadID)
	if err != nil {
		return codex.TurnOptions{}, err
	}
	summary, err := session.Find(s.config.Workspace, owner.Slug)
	if err != nil || summary.Codex.ThreadID != owner.RootThreadID {
		return codex.TurnOptions{}, errors.New("lead identity changed")
	}
	instructions := ""
	roster, err := s.loadTeamRoster(summary)
	if err == nil {
		instructions = roster.LeadInstructions
	} else if !errors.Is(err, os.ErrNotExist) {
		return codex.TurnOptions{}, err
	}
	return codex.TurnOptions{Model: settings.Model, ReasoningEffort: settings.ReasoningEffort,
		ThreadPolicy: workspacecodex.LeadThreadPolicy(owner.Slug, s.config.Workspace, instructions)}, nil
}

func (s *Server) startMonitorOperations() {
	service, err := s.monitorDeliveryService()
	if err != nil {
		return
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		nextOperation, nextEvent := 0, 0
		lastError := ""
		for {
			select {
			case <-s.operationContext.Done():
				return
			case <-ticker.C:
			}
			batchErr := s.runMonitorOperations(s.operationContext, service, &nextOperation, &nextEvent, 20*time.Second, 5*time.Second)
			if batchErr != nil && batchErr.Error() != lastError {
				s.config.Logger.Printf("monitor operations: %v", batchErr)
				lastError = batchErr.Error()
			} else if batchErr == nil {
				lastError = ""
			}
		}
	}()
}

// Saved outcomes get an independent budget before utility observation. Bound
// each item as well, so one unavailable operation cannot consume the whole pass
// repeatedly. Both cursors advance before waiting, including on timeout.
func (s *Server) runMonitorOperations(ctx context.Context, service *monitoroperation.DeliveryService, nextOperation, nextEvent *int, batchTimeout, itemTimeout time.Duration) error {
	var batchErr error
	delivery, cancelDelivery := context.WithTimeout(ctx, batchTimeout)
	refs, err := service.Pending(delivery)
	if err != nil {
		batchErr = err
	}
	for count := 0; count < len(refs) && count < 16 && delivery.Err() == nil; count++ {
		ref := refs[*nextEvent%len(refs)]
		*nextEvent++
		item, cancel := context.WithTimeout(delivery, itemTimeout)
		if err := s.withMonitorGeneration(item, func() error { return service.Dispatch(item, ref.OperationID, ref.EventID) }); err != nil {
			batchErr = err
		}
		cancel()
	}
	cancelDelivery()

	observation, cancelObservation := context.WithTimeout(ctx, batchTimeout)
	defer cancelObservation()
	ops, err := service.Store.List(observation, "")
	if err != nil {
		batchErr = err
	}
	for count := 0; count < len(ops) && count < 16 && observation.Err() == nil; count++ {
		op := ops[*nextOperation%len(ops)]
		*nextOperation++
		item, cancel := context.WithTimeout(observation, itemTimeout)
		if err := s.withMonitorGeneration(item, func() error { return service.Observe(item, op.ID) }); err != nil {
			batchErr = err
		}
		cancel()
	}
	return batchErr
}

// Use the operation context for transition-lock waits as well as native reads,
// so portal shutdown does not wait for an unrelated generation admission.
func (s *Server) withMonitorGeneration(ctx context.Context, run func() error) error {
	unlock, err := s.lockTransitionContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.requireCurrentHostProfile(); err != nil {
		return err
	}
	return run()
}
