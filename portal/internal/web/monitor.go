package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
		Client: monitorDeliveryClient{DeliveryClient: client, activity: s.activity},
		Admission: func(owner monitoroperation.Owner) error {
			if err := s.requireMaintenanceAdmission(); err != nil {
				return err
			}
			return s.requireProviderAdmission(owner.Slug)
		},
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
	ciService, err := s.ciMonitorService(service)
	if err != nil {
		s.config.Logger.Printf("monitor CI owner: %v", err)
	}
	s.operationWG.Add(1)
	go func() {
		defer s.operationWG.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		nextOperation, nextEvent, nextObserver := 0, 0, 0
		lastError := ""
		for {
			select {
			case <-s.operationContext.Done():
				return
			case <-ticker.C:
			}
			batchErr := s.runMonitorOperations(s.operationContext, service, &nextOperation, &nextEvent, 20*time.Second, 5*time.Second)
			if ciService != nil {
				if err := s.runCIObservations(s.operationContext, ciService, &nextObserver, 20*time.Second, 5*time.Second); err != nil {
					batchErr = errors.Join(batchErr, err)
				}
			}
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

func (s *Server) ciController() (*ciPollController, error) {
	s.monitorCIOnce.Do(func() {
		var epoch [16]byte
		if _, err := rand.Read(epoch[:]); err != nil {
			s.monitorCIError = err
			return
		}
		s.monitorCI = &ciPollController{}
		s.monitorCIEpoch = hex.EncodeToString(epoch[:])
	})
	return s.monitorCI, s.monitorCIError
}

func (s *Server) ciMonitorService(delivery *monitoroperation.DeliveryService) (*monitoroperation.Service, error) {
	if _, err := s.ciController(); err != nil {
		return nil, err
	}
	return &monitoroperation.Service{Store: delivery.Store, ObserverEpoch: s.monitorCIEpoch,
		Resolve: delivery.Resolve, LockSession: delivery.LockSession,
		Admit: func(_ context.Context, owner monitoroperation.Owner) error {
			if err := s.requireMaintenanceAdmission(); err != nil {
				return err
			}
			return s.requireProviderAdmission(owner.Slug)
		},
	}, nil
}

// The existing worker owns the bounded request and publishes its producer
// receipt even when observation times out or the portal begins closing. Record
// reconciliation never launches a request and cannot deliver after detachment.
func (s *Server) runCIObservations(ctx context.Context, service *monitoroperation.Service, cursor *int, batchTimeout, itemTimeout time.Duration) error {
	batch, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	records, err := service.Store.ListObservers(batch, "")
	if err != nil {
		return err
	}
	controller, err := s.ciController()
	if err != nil {
		return err
	}
	var batchErr error
	for count := 0; count < len(records) && count < 16 && batch.Err() == nil; count++ {
		record := records[*cursor%len(records)]
		*cursor++
		if record.Driver != "direct-ci" || record.Phase != "observing" {
			continue
		}
		item, stop := context.WithTimeout(batch, itemTimeout)
		err = s.withMonitorGeneration(item, func() error {
			claim, err := service.ClaimCIPoll(item, record.ID)
			if errors.Is(err, monitoroperation.ErrPollNotDue) {
				return nil
			}
			if err != nil {
				return err
			}
			publish := func(receipt monitoroperation.PollReceipt) error {
				save, closeSave := context.WithTimeout(context.WithoutCancel(item), 5*time.Second)
				defer closeSave()
				_, err := service.CommitCIPoll(save, claim, nil, receipt, "")
				return err
			}
			admit := func() error {
				unlock, err := service.LockSession(item, claim.Owner.Slug)
				if err != nil {
					return err
				}
				defer unlock()
				owner, err := service.Resolve(claim.Owner.Slug)
				if err != nil {
					return err
				}
				if !monitoroperation.SameOwner(owner, claim.Owner) {
					return errors.New("CI poll owner changed before start")
				}
				if err := service.Admit(item, owner); err != nil {
					return err
				}
				current, err := service.Store.GetObserver(item, claim.ID)
				if err != nil {
					return err
				}
				if current.Phase != "observing" || current.SourceDigest != claim.SourceDigest || current.Poll == nil || current.Poll.ID != claim.Poll.ID || current.Poll.OwnerEpoch != claim.Poll.OwnerEpoch {
					return errors.New("CI poll admission changed before start")
				}
				return nil
			}
			observer := ciPollObserver{controller: controller, claim: claim, executable: s.config.GH, publish: publish, admit: admit}
			snapshot, receipt, pollErr := observer.Poll(item, claim.Source)
			save, closeSave := context.WithTimeout(context.WithoutCancel(item), 5*time.Second)
			defer closeSave()
			var remote *monitoroperation.CISnapshot
			if pollErr == nil {
				remote = &snapshot
			}
			detail := ""
			if pollErr != nil {
				detail = boundedLifecycleError(pollErr.Error())
			}
			_, saveErr := service.CommitCIPoll(save, claim, remote, receipt, detail)
			return errors.Join(pollErr, saveErr)
		})
		stop()
		if err != nil {
			batchErr = errors.Join(batchErr, err)
		}
	}
	return batchErr
}
