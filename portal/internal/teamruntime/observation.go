package teamruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// ObservationClient is deliberately small and explicit. Observation does not
// depend on roster mutation or private client submission storage.
type ObservationClient interface {
	ProveArchivedRootThread(context.Context, string, string) (workspacecodex.ArchiveState, error)
	ObserveThreadIdentity(context.Context, string, string, string, workspacecodex.ArchiveState) (workspacecodex.ThreadObservation, error)
}

type ObservationSubject struct {
	Address        string `json:"address"`
	ThreadID       string `json:"threadId"`
	ProjectID      string `json:"projectId,omitempty"`
	ActivityToken  string `json:"activityToken"`
	LastActivityAt *int64 `json:"lastActivityAt"`
	Idle           bool   `json:"idle"`
}

type Observation struct {
	Schema         int                                    `json:"schema"`
	Workspace      string                                 `json:"workspace"`
	Slug           string                                 `json:"slug"`
	Identity       string                                 `json:"identity"`
	ActivityKnown  bool                                   `json:"activityKnown"`
	ActivityToken  string                                 `json:"activityToken"`
	LastActivityAt *int64                                 `json:"lastActivityAt"`
	Idle           bool                                   `json:"idle"`
	Subjects       []ObservationSubject                   `json:"subjects"`
	Diagnostics    []workspacecodex.ObservationDiagnostic `json:"diagnostics"`
}

func (result *Observation) Unknown(code, message string) {
	result.ActivityKnown, result.Idle, result.ActivityToken, result.LastActivityAt = false, false, "", nil
	result.Diagnostics = append(result.Diagnostics, workspacecodex.ObservationDiagnostic{Code: code, Category: "activity_unknown", Message: message})
}

func retainedObservationIdentities(roster *Roster) [][3]string {
	identities := [][3]string{}
	for _, member := range roster.Members {
		if member.State != "removed" {
			identities = append(identities, [3]string{member.Address, member.Thread, member.ProjectID})
		}
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i][0] < identities[j][0] })
	return identities
}

// ObserveRetained accepts an exact root already selected by a manifest or a
// reviewed migration identity. The latter needs no temporary manifest and gets
// the same roster, project, persistence and unknown-writer proofs.
func (service Service) ObserveRetained(ctx context.Context, slug, rootThreadID, identity string) (Observation, error) {
	if service.Store == nil || service.Client == nil {
		return Observation{}, errors.New("retained observation runtime is unavailable")
	}
	result := Observation{Schema: 1, Workspace: service.Store.workspace, Slug: slug, Identity: identity,
		ActivityKnown: true, Idle: true, Subjects: []ObservationSubject{}, Diagnostics: []workspacecodex.ObservationDiagnostic{}}
	observer, ok := service.Client.(ObservationClient)
	if !ok {
		return result, errors.New("semantic conversation observer is unavailable")
	}
	err := service.Store.withOperationLock(ctx, slug, func() error {
		load := func() (*Roster, error) {
			roster, err := service.Store.Load(slug, rootThreadID)
			if errors.Is(err, os.ErrNotExist) {
				return &Roster{}, nil
			}
			return roster, err
		}
		roster, err := load()
		if err != nil {
			return err
		}
		identities := retainedObservationIdentities(roster)
		cwd := filepath.Join(service.Store.workspace, "work", slug)
		rootState, err := observer.ProveArchivedRootThread(ctx, rootThreadID, cwd)
		if err != nil {
			return err
		}
		active := map[string]string{}
		if rootState != workspacecodex.ArchiveArchived {
			active[rootThreadID] = ""
		}
		add := func(address, thread, project string, state workspacecodex.ArchiveState) error {
			observation, err := observer.ObserveThreadIdentity(ctx, thread, cwd, project, state)
			if err != nil {
				return err
			}
			if observation.Schema != 1 || observation.ThreadID != thread || observation.Cwd != cwd || !observation.ActivityKnown || observation.ActivityToken == "" {
				return errors.New("retained observation returned invalid subject evidence")
			}
			result.Subjects = append(result.Subjects, ObservationSubject{Address: address, ThreadID: thread, ProjectID: project,
				ActivityToken: observation.ActivityToken, LastActivityAt: observation.LastActivityAt, Idle: observation.Idle})
			result.Idle = result.Idle && observation.Idle
			result.Diagnostics = append(result.Diagnostics, observation.Diagnostics...)
			return nil
		}
		if err := add("lead", rootThreadID, "", rootState); err != nil {
			return err
		}
		members := append([]Member(nil), roster.Members...)
		sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })
		for _, member := range members {
			if member.State == "removed" {
				continue
			}
			archived, err := service.retainedMemberArchiveState(ctx, member, cwd)
			if err != nil {
				return fmt.Errorf("retained member %s cannot be verified: %w", member.Address, err)
			}
			state := workspacecodex.ArchiveActive
			if archived {
				state = workspacecodex.ArchiveArchived
			}
			if err := add(member.Address, member.Thread, member.ProjectID, state); err != nil {
				return err
			}
			again, err := service.retainedMemberArchiveState(ctx, member, cwd)
			if err != nil || archived != again {
				return errors.New("retained member identity changed during observation")
			}
			if !archived {
				active[member.Thread] = member.ProjectID
			}
		}
		if err := workspacecodex.RequireExactActiveConversations(ctx, service.Client, cwd, rootThreadID, active); err != nil {
			return err
		}
		rootAgain, err := observer.ProveArchivedRootThread(ctx, rootThreadID, cwd)
		if err != nil || rootAgain != rootState {
			return errors.New("retained root changed during observation")
		}
		after, err := load()
		if err != nil || !reflect.DeepEqual(identities, retainedObservationIdentities(after)) {
			return errors.New("retained team identity changed during observation")
		}
		for _, member := range after.Members {
			if member.State != "removed" {
				if _, err := service.retainedMemberArchiveState(ctx, member, cwd); err != nil {
					return err
				}
			}
		}
		// One unknown historical timestamp makes the aggregate date unavailable;
		// trustworthy tokens still allow observation-based grace.
		allDates := true
		for _, subject := range result.Subjects {
			if subject.LastActivityAt == nil {
				allDates = false
				continue
			}
			if result.LastActivityAt == nil || *subject.LastActivityAt > *result.LastActivityAt {
				value := *subject.LastActivityAt
				result.LastActivityAt = &value
			}
		}
		if !allDates {
			result.LastActivityAt = nil
		}
		result.ActivityToken = workspacecodex.ObservationToken([]any{1, identities, result.Subjects})
		return nil
	})
	if err != nil {
		code := "retained_set_unverified"
		var failure *workspacecodex.ObservationError
		if errors.As(err, &failure) {
			code = failure.Code
		}
		result.Unknown(code, "Exact retained conversation activity cannot be verified.")
	}
	return result, nil
}

// RequireNoRoster treats even an empty retained roster as threadless residue.
func (store *Store) RequireNoRoster(slug string) error {
	if !session.ValidSlug(slug) {
		return errors.New("invalid observation session")
	}
	_, err := os.Lstat(store.path(slug))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("retained team state contradicts threadless tracking")
}
