package web

import (
	"context"
	"errors"
	"os"
	"reflect"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"golang.org/x/sys/unix"
)

func (s *Server) initializePiFork(receipt creationReceipt) error {
	ctx, cancel := context.WithTimeout(s.operationContext, 4*time.Minute)
	defer cancel()
	transition, release, err := s.acquireTransitionContext(ctx, unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer release()
	if err = s.requireCurrentHostProfile(); err != nil {
		return err
	}
	if s.provePiCreation(receipt) == nil {
		return nil
	}
	fork, ok := s.config.PiCreation.Runtime.(piruntime.ForkController)
	if !ok {
		return piruntime.ErrUnavailable
	}
	if err = s.preparePiCreation(ctx, receipt); err != nil {
		return err
	}
	var binding session.BackendBinding
	if err = s.withPiCreationOwner(ctx, receipt, func(i session.PiCreationIntent) error {
		var e error
		binding, e = (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: i.Workspace}).Load(i.Slug)
		return e
	}); err != nil {
		return err
	}
	if _, err = s.piTrackingPhase(ctx, transition, receipt, "prepare-tracking", binding, ""); err != nil {
		return err
	}
	var handle agentbackend.ExecutionHandle
	err = s.withPiCreationOwner(ctx, receipt, func(i session.PiCreationIntent) error {
		store := session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: i.Workspace}
		var e error
		binding, e = store.Load(i.Slug)
		if e != nil {
			return e
		}
		if e = i.ValidateBinding(binding); e != nil {
			return e
		}
		journal, e := readPiCreationJournal(piJournalPath(i.Workspace, i.Slug))
		if e != nil {
			return e
		}
		start, e := s.config.PiCreation.StartRequest(i, binding.Revision)
		if e != nil {
			return e
		}
		if start.Session != i.SessionRef() || start.RequestID != i.RuntimeRequestID() || start.BindingRevision != 1 {
			return agentbackend.ErrBindingConflict
		}
		if start.AppStorePath != s.config.PackageRoot {
			if e = admitPiFrozenPackage(s.config, s.hostProfile, i, start); e != nil {
				return e
			}
		}
		status, e := s.piCreationRuntimeReadyOwned(ctx, start, journal.NativeGeneration, !journal.NativeCreateStarted)
		if e != nil {
			return e
		}
		if e = s.piWorkflows.ensureAttachment(ctx, status.Runtime); e != nil {
			return e
		}
		authority := session.PiRuntimeAuthority{Schema: 2, Backend: agentbackend.Pi, BackendContract: 1, Workspace: i.Workspace, Slug: i.Slug, SessionIncarnation: i.SessionIncarnation, BindingRevision: binding.Revision, Generation: status.Runtime.Generation, State: "starting"}
		if binding.State == "ready" {
			authority.State = "ready"
		}
		if e = session.PublishPiRuntimeAuthority(s.config.AuthorityDir, binding, authority); e != nil {
			return e
		}
		if !journal.NativeCreateStarted {
			journal.NativeCreateStarted = true
			journal.NativeGeneration = status.Runtime.Generation
			if e = writePiCreationRecord(piJournalPath(i.Workspace, i.Slug), journal, i.RecordLimit()); e != nil {
				return e
			}
		}
		// New desired policy has its own revision-one identity. Applied copied settings
		// retain the historical source policy digest until an explicit Configure.
		root, e := piruntime.SessionStateDirectory(s.config.UserStateRoot, i.SessionRef())
		if e != nil {
			return e
		}
		policies := piproviders.Store{Root: root, Catalog: s.config.PiCreation.Catalog, Session: i.SessionRef()}
		if policy, e := policies.Load(); errors.Is(e, os.ErrNotExist) {
			if e = policies.CompareAndSwap(0, i.ProviderSelection.Policy.Public()); e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else if !reflect.DeepEqual(policy, i.ProviderSelection.Policy.Public()) {
			return piruntime.ErrConflict
		}
		proofs := map[string]piruntime.ForkProof{}
		for n, m := range i.Fork.Members {
			if m.Origin == nil {
				continue
			}
			h := agentbackend.ExecutionHandle{Conversation: agentbackend.ConversationRef{Session: i.SessionRef(), Backend: agentbackend.Pi, Member: m.Address, Incarnation: i.Conversations[n].Incarnation, NativeID: m.NativeID}, Generation: status.Runtime.Generation}
			// A completed destination is verified first and never depends on the source.
			proof, verifyErr := fork.VerifyFork(ctx, h, m.CreationID, *m.Origin)
			request := piruntime.ForkRequest{Source: m.Origin.Source, Snapshot: m.Origin.Snapshot, NativeID: m.NativeID, Destination: piruntime.CreateRequest{Session: i.SessionRef(), Member: m.Address, Incarnation: i.Conversations[n].Incarnation, CreationID: m.CreationID, Instructions: m.Origin.Snapshot.Instructions, Settings: m.Origin.Snapshot.Settings, ToolPolicy: m.Origin.Snapshot.ToolPolicy}}
			found := verifyErr == nil
			if !found {
				proof, found, e = fork.RecoverFork(ctx, status.Runtime, request)
				if e != nil {
					return e
				}
			}
			if !found {
				sourceLock, e := session.LockRuntimeExclusive(s.config.AuthorityDir, i.Fork.Source.Slug)
				if e != nil {
					return e
				}
				source, e := s.piSessionAdmissionLockedContext(ctx, i.Fork.Source)
				if e != nil {
					sourceLock.Close()
					return e
				}
				release := func() { sourceLock.Close() }
				e = func() error {
					defer release()
					rosters, e := teamruntime.NewStore(s.config.UserStateRoot, i.Workspace)
					if e != nil {
						return e
					}
					roster, rosterErr := rosters.LoadPi(i.Fork.Source.Slug)
					if i.Fork.SourceRosterRevision == 0 {
						if !errors.Is(rosterErr, os.ErrNotExist) {
							return piruntime.ErrConflict
						}
					} else if rosterErr != nil || roster.Revision != i.Fork.SourceRosterRevision {
						return piruntime.ErrConflict
					}
					if source.Binding.Revision != i.Fork.SourceBindingRevision || source.intent.ReceiptID != i.Fork.SourceCreationID {
						return agentbackend.ErrBindingConflict
					}
					for _, saved := range i.Fork.Members {
						if saved.State != "removed" {
							if e := s.validatePiForkSourcePolicy(i, saved, source, roster); e != nil {
								return e
							}
						}
					}
					proof, e = fork.Fork(ctx, status.Runtime, request)
					return e
				}()
				if e != nil {
					return e
				}
			}
			if e = validatePiForkProof(h, m.CreationID, m.NativeID, *m.Origin, proof); e != nil {
				return e
			}
			if proof.Settings != m.Origin.Snapshot.Settings {
				return piruntime.ErrConflict
			}
			proofs[m.Address] = proof
			if n == 0 {
				handle = h
				journal.Native = &session.PiCreationNative{NativeID: m.NativeID, HistoryRef: proof.HistoryRef}
			}
		}
		// Publish no ready binding until the entire frozen materialized team is proved.
		if journal.Native == nil {
			return piruntime.ErrHeld
		}
		if e = writePiCreationRecord(piJournalPath(i.Workspace, i.Slug), journal, i.RecordLimit()); e != nil {
			return e
		}
		if binding.State != "ready" {
			next := binding
			next.Conversations = append([]session.BackendConversation{}, binding.Conversations...)
			next.Revision++
			next.State = "ready"
			for n, m := range i.Fork.Members {
				if m.Origin != nil {
					next.Conversations[n].State = "ready"
					next.Conversations[n].NativeID = m.NativeID
				}
			}
			binding, e = store.CompareAndSwap(ctx, binding.Revision, next)
			if e != nil {
				return e
			}
		}
		authority.BindingRevision = binding.Revision
		authority.State = "ready"
		if e = session.PublishPiRuntimeAuthority(s.config.AuthorityDir, binding, authority); e != nil {
			return e
		}
		return s.publishPiForkRoster(ctx, i, binding, authority.Generation, proofs)
	})
	if err != nil {
		return err
	}
	completed, err := s.piTrackingPhase(ctx, transition, receipt, "complete-tracking", binding, handle.Generation)
	if err != nil {
		return err
	}
	return s.withPiCreationOwner(ctx, receipt, func(i session.PiCreationIntent) error {
		if e := s.verifyPiForkMembers(ctx, i, binding, handle.Generation); e != nil {
			return e
		}
		if e := s.validatePiTracking(i, completed, true); e != nil {
			return e
		}
		evidence := piCreationEvidence{Schema: 5, Workspace: i.Workspace, Slug: i.Slug, Backend: agentbackend.Pi, BackendContract: 1, ReceiptID: i.ReceiptID, SessionIncarnation: i.SessionIncarnation, BindingRevision: binding.Revision, Conversations: i.Conversations, NativeID: handle.Conversation.NativeID, Model: i.Model, Effort: i.Effort, Provider: i.Provider, ProviderSelection: i.ProviderSelection, CatalogDigest: i.DirectTeam.CatalogDigest, TeamDigest: i.DirectTeam.TeamDigest, Team: i.DirectTeam.ID, TrackingDevice: completed.TrackingDevice, TrackingInode: completed.TrackingInode, Fork: i.Fork}
		if old, e := s.readPiCreationEvidence(receipt); e == nil {
			if !reflect.DeepEqual(old, evidence) {
				return piruntime.ErrConflict
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e := writePiCreationRecord(s.creationEvidencePath(receipt), evidence, maxCreationReceiptBytes); e != nil {
			return e
		}
		journal, e := readPiCreationJournal(piJournalPath(i.Workspace, i.Slug))
		if e != nil {
			return e
		}
		journal.State = "ready"
		return writePiCreationRecord(piJournalPath(i.Workspace, i.Slug), journal, i.RecordLimit())
	})
}
func (s *Server) validatePiForkSourcePolicy(i session.PiCreationIntent, m session.PiForkMember, current piSessionAdmission, roster teamruntime.PiRoster) error {
	root, e := piruntime.SessionStateDirectory(s.config.UserStateRoot, i.Fork.Source)
	if e != nil {
		return e
	}
	policy, e := (piproviders.Store{Root: root, Catalog: s.config.PiCreation.Catalog, Session: i.Fork.Source}).Load()
	stored := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if stored != m.PolicyStored {
		return piruntime.ErrHeld
	}
	var member *teamruntime.PiRosterMember
	for n := range roster.Members {
		if roster.Members[n].Address == m.Address && roster.Members[n].Incarnation == m.SourceIncarnation {
			member = &roster.Members[n]
		}
	}
	bound := session.BackendConversation{Address: m.Address, Incarnation: m.SourceIncarnation}
	value, source, catalog, revision := s.piForkSourceSelection(current.intent, bound, member, policy, stored)
	if revision != m.PolicyRevision || catalog != m.CatalogDigest || source != m.SelectionSource || !reflect.DeepEqual(&value, m.Selection) {
		return piruntime.ErrConflict
	}
	return nil
}
func (s *Server) verifyPiForkMembers(ctx context.Context, i session.PiCreationIntent, b session.BackendBinding, generation string, retained ...bool) error {
	fork, ok := s.config.PiCreation.Runtime.(piruntime.ForkController)
	if !ok {
		return piruntime.ErrUnavailable
	}
	removed := map[string]bool{}
	if len(retained) == 1 && retained[0] {
		receipt := creationReceipt{ReceiptID: i.ReceiptID, Request: creationRequest{Slug: i.Slug}}
		evidence, e := s.readPiCreationEvidence(receipt)
		if e != nil || validatePiForkEvidence(i, b, evidence) != nil {
			return piruntime.ErrHeld
		}
		store, e := teamruntime.NewStore(s.config.UserStateRoot, i.Workspace)
		if e != nil {
			return e
		}
		roster, e := store.LoadPi(i.Slug)
		if e == nil && roster.Schema == 5 {
			if validatePiExtendedBinding(i, b, roster) != nil {
				return piruntime.ErrHeld
			}
			for _, m := range roster.Members {
				if m.State == "removed" {
					removed[m.Address] = true
				}
			}
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	for _, m := range i.Fork.Members {
		if m.Origin == nil || removed[m.Address] {
			continue
		}
		ref, e := b.Resolve(m.Address)
		if e != nil {
			return e
		}
		h := agentbackend.ExecutionHandle{Conversation: ref, Generation: generation}
		p, e := fork.VerifyFork(ctx, h, m.CreationID, *m.Origin)
		if e != nil {
			return e
		}
		if e = validatePiForkProof(h, m.CreationID, m.NativeID, *m.Origin, p); e != nil {
			return e
		}
	}
	return nil
}

type piForkRosterVerifier struct {
	server     *Server
	intent     session.PiCreationIntent
	binding    session.BackendBinding
	generation string
}

func (v piForkRosterVerifier) VerifyPiRosterReady(ctx context.Context, r teamruntime.PiRoster) error {
	if r.Schema != 4 || r.ForkCreationID != v.intent.ReceiptID || r.Scope().Ref() != v.intent.SessionRef() {
		return piruntime.ErrScope
	}
	return v.server.verifyPiForkMembers(ctx, v.intent, v.binding, v.generation)
}
func (s *Server) publishPiForkRoster(ctx context.Context, i session.PiCreationIntent, b session.BackendBinding, generation string, proofs map[string]piruntime.ForkProof) error {
	store, e := teamruntime.NewStore(s.config.UserStateRoot, i.Workspace)
	if e != nil {
		return e
	}
	if r, e := store.LoadPi(i.Slug); e == nil {
		return validatePiExtendedBinding(i, b, r)
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	now := time.Now().UTC()
	r := teamruntime.PiRoster{Schema: 4, ForkCreationID: i.ReceiptID, Workspace: i.Workspace, Slug: i.Slug, Revision: 1, CreatedAt: now, UpdatedAt: now, PresetID: i.DirectTeam.ID, CatalogDigest: i.DirectTeam.CatalogDigest, TeamDigest: i.DirectTeam.TeamDigest, LeadModel: i.Model, LeadEffort: i.Effort, LeadInstructions: i.DirectTeam.LeadInstructions, Members: []teamruntime.PiRosterMember{}, Backend: "pi", BackendContract: 1, SessionIncarnation: i.SessionIncarnation, LeadIncarnation: i.Conversations[0].Incarnation}
	for n, saved := range i.DirectTeam.Members {
		m := i.Fork.Members[n+1]
		added, e := time.Parse(time.RFC3339Nano, m.AddedAt)
		if e != nil {
			return e
		}
		var removed *time.Time
		if m.RemovedAt != "" {
			x, e := time.Parse(time.RFC3339Nano, m.RemovedAt)
			if e != nil {
				return e
			}
			removed = &x
		}
		// Address suffix is preserved even when prior members were removed.
		var index uint64
		for _, c := range saved.Address[len(saved.Role):] {
			index = index*10 + uint64(c-'0')
		}
		r.Members = append(r.Members, teamruntime.PiRosterMember{Address: saved.Address, Role: saved.Role, Index: index, Model: saved.Model, Effort: saved.Effort, Behavior: saved.Behavior, Purpose: saved.Purpose, Instructions: saved.Instructions, Access: saved.Access, PolicyCatalogDigest: i.DirectTeam.CatalogDigest, State: m.State, AddedAt: added, RemovedAt: removed, Incarnation: i.Conversations[n+1].Incarnation, CreationID: m.CreationID, Fork: piForkReference(i, m)})
	}
	admission := piCreationRosterAdmission{ref: i.SessionRef(), generation: generation, validate: s.requireCurrentHostProfile}
	return store.SavePi(ctx, admission, generation, 0, r, piForkRosterVerifier{s, i, b, generation})
}

func piForkReference(i session.PiCreationIntent, m session.PiForkMember) *teamruntime.PiForkReference {
	if m.Origin == nil {
		return nil
	}
	return &teamruntime.PiForkReference{RootCreationID: i.ReceiptID, NativeID: m.NativeID}
}
