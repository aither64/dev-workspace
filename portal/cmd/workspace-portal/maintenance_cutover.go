package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"os"
	"path/filepath"
)

func (m maintenanceControl) destinationIdentity(o maintenance.Operation, p maintenance.Participant) (string, error) {
	s, e := m.service(o, p)
	if e != nil {
		return "", e
	}
	epoch, e := m.destinationServiceIdentity(o, s)
	if e != nil {
		return "", e
	}
	inc, e := trackingIncarnation(p.Workspace, p.Slug)
	if e != nil || inc != p.Incarnation {
		return "", errors.New("destination incarnation changed")
	}
	if e = m.retainedIdentity(p, s); e != nil {
		return "", e
	}

	return epoch, nil
}

func (m maintenanceControl) destinationServiceIdentity(o maintenance.Operation, s maintenance.Service) (string, error) {
	epoch, e := session.SocketIdentity(s.Socket)
	if e != nil || epoch == s.SocketEpoch || epoch != s.DestinationEpoch {
		return "", errors.New("destination native epoch is unproved")
	}
	process, processErr := processIdentity(s.DestinationPID)
	exe, exeErr := os.Readlink(fmt.Sprintf("/proc/%d/exe", s.DestinationPID))
	if processErr != nil || exeErr != nil || process != s.DestinationProcessIdentity || exe != s.DestinationNative {
		return "", errors.New("destination process changed")
	}
	selected, err := filepath.EvalSymlinks(m.store.Profile)
	if err != nil || selected != o.ActivationPackage {
		return "", errors.New("destination selected package changed")
	}
	var marker struct {
		Launch struct {
			Native string `json:"codex_path"`
		} `json:"launch"`
	}
	markerData, err := os.ReadFile(filepath.Join(filepath.Dir(s.Socket), "registration.json"))
	if err != nil || len(markerData) > 4<<20 || json.Unmarshal(markerData, &marker) != nil || marker.Launch.Native != o.ActivationNative {
		return "", errors.New("destination native launch changed")
	}
	return epoch, nil
}

const maintenanceResumeText = "Resume the work paused for the workspace update. Inspect your saved checkpoint, current state and partial effects before retrying any action. Restart an interrupted child only if you decide it is needed."

func (m maintenanceControl) continueParticipants(ctx context.Context, id string) (maintenance.Response, error) {
	o, err := m.store.Load(id)
	if err != nil {
		return maintenance.Response{}, err
	}
	if o.ContinuationsSettled {
		return maintenance.Response{Schema: 1, Status: "continued"}, nil
	}
	if !o.CancelRequested {
		if !o.SourcesStopped || !o.ActivationIntent {
			return maintenance.Response{}, errors.New("activation not completed")
		}
		for _, s := range o.Services {
			if _, err := m.destinationServiceIdentity(o, s); err != nil {
				return maintenance.Response{}, err
			}
		}
	}
	if _, err = m.update(ctx, id, 0, func(current *maintenance.Operation) error { current.Phase = "resuming"; return nil }); err != nil {
		return maintenance.Response{}, err
	}
	for index, p := range o.Participants {
		if p.ContinuationDisposition == "accepted" || p.ContinuationDisposition == "suppressed" {
			continue
		}
		var epoch string
		if o.CancelRequested {
			err = m.identity(ctx, o, p)
			epoch = p.SocketEpoch
		} else {
			epoch, err = m.destinationIdentity(o, p)
		}
		if err != nil {
			return maintenance.Response{}, err
		}
		c := m.client(o, p)
		disposition := "pending"
		if p.ContinuationDisposition == "submitting" || p.ContinuationDisposition == "unknown" {
			_, found, reconcileErr := c.ReconcileSendWithOptions(ctx, p.Thread, maintenanceResumeText, p.ContinuationID, "maintenance-continuation:"+id, codex.TurnOptions{})
			if reconcileErr != nil {
				c.Close()
				return maintenance.Response{Schema: 1, Status: "recovery-required", Reason: "continuation-outcome-unknown"}, nil
			}
			if found {
				disposition = "accepted"
			}
			// An absent/prepared attempt can retry only through the same stable sender;
			// that sender owns uncertainty and never manufactures a new submission ID.
		} else {
			claimed, claimErr := (maintenance.Controller{Store: m.store}).AdmitContinuation(ctx, id, p.Thread, p.Incarnation)
			if claimErr != nil {
				c.Close()
				return maintenance.Response{}, claimErr
			}
			if claimed.Status != "admitted" {
				c.Close()
				continue
			}
		}
		if disposition != "accepted" {
			current, loadErr := m.store.Load(id)
			if loadErr != nil {
				c.Close()
				return maintenance.Response{}, loadErr
			}
			if current.Participants[index].StopRevision != 0 {
				disposition = "suppressed"
			} else {
				service, _ := m.service(o, p)
				recovery := session.RecoveryStore{Root: m.store.Root, Workspace: p.Workspace}
				if _, err = recovery.Load(p.Slug); errors.Is(err, os.ErrNotExist) {
					err = recovery.Set(ctx, p.Slug, p.Root, service.Socket, true, false)
				}
				if err == nil {
					err = recovery.Update(ctx, p.Slug, func(record *session.Recovery) error {
						if record.ThreadID != p.Root || record.SocketPath != service.Socket {
							return errors.New("maintenance recovery root changed")
						}
						if record.SocketIdentity != epoch {
							record.ActiveThreads = []string{}
						}
						record.SocketIdentity = epoch
						record.ActiveThreads = append(record.ActiveThreads, p.Thread)
						return nil
					})
				}
				if err == nil {
					err = c.ActivateThread(ctx, p.Thread, codex.ThreadPolicy{})
				}
				if err != nil {
					c.Close()
					return maintenance.Response{}, err
				}
				_, err = c.SendWithOptions(ctx, p.Thread, maintenanceResumeText, p.ContinuationID, "maintenance-continuation:"+id, codex.TurnOptions{})
				disposition = "accepted"
				if err != nil {
					disposition = "unknown"
					var unsent *codex.MessageNotSentError
					if errors.As(err, &unsent) {
						disposition = "pending"
					}
				}
			}
		}
		c.Close()
		_, saveErr := m.update(ctx, id, 0, func(current *maintenance.Operation) error {
			q := &current.Participants[index]
			q.ContinuationDisposition = disposition
			if q.StopRevision != 0 && disposition == "pending" {
				q.ContinuationDisposition = "suppressed"
			}
			return nil
		})
		if saveErr != nil {
			return maintenance.Response{}, saveErr
		}
		if err != nil {
			return maintenance.Response{Schema: 1, Status: "recovery-required", Reason: "continuation-outcome-unknown"}, nil
		}
	}
	_, err = m.update(ctx, id, 0, func(current *maintenance.Operation) error {
		for _, p := range current.Participants {
			if p.ContinuationDisposition != "accepted" && p.ContinuationDisposition != "suppressed" {
				return errors.New("continuation remains unresolved")
			}
		}
		current.ContinuationsSettled = true
		current.Phase = "resuming"
		return nil
	})
	return maintenance.Response{Schema: 1, Status: "continued"}, err
}
