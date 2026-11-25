package teamruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
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
	ArchiveState   string `json:"archiveState,omitempty"`
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

// Current subjects and sealed history owners are distinct. Only current ready
// routes can ever become execution principals; this set grants observation only.
type retainedObservationRoute struct {
	Address, Member, Thread, Project string
	History                          bool
	Binding                          codexproviders.ResolvedBinding
}

func (store *Store) retainedObservationRoutes(slug, root string, roster *Roster) ([]retainedObservationRoute, error) {
	summary, err := session.Find(store.workspace, slug)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	marked := err == nil && summary.Codex.BindingSchema == 3
	rows := []Member{{Address: "lead", Thread: root, State: "ready"}}
	members := append([]Member(nil), roster.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Address < members[j].Address })
	for _, member := range members {
		if member.State != "removed" {
			rows = append(rows, member)
		}
	}
	routes := []retainedObservationRoute{}
	seen := map[string]bool{}
	for _, member := range rows {
		if member.Thread == "" || member.RetireIntent != "" || (member.State != "ready" && member.State != "archived") {
			return nil, errors.New("retained member has no stable identity")
		}
		resolved := codexproviders.ResolvedBinding{ThreadID: member.Thread}
		targets := []codexproviders.ReceiptTarget{{ThreadID: member.Thread}}
		if marked {
			resolved, err = store.ResolveHistoryConversation(slug, root, member.Address, member.Thread)
			if err != nil {
				return nil, err
			}
			state, err := store.codexBindingState(summary)
			if err != nil {
				return nil, err
			}
			targets, err = state.ReceiptTargets(resolved.Key, true, func(op string) ([]byte, error) {
				return codexproviders.ReadBindingEvidence(store.stateRoot, state.Workspace, op)
			})
			if err != nil {
				return nil, err
			}
		}
		for i, target := range targets {
			if seen[target.ThreadID] {
				return nil, errors.New("retained routes duplicate native ownership")
			}
			seen[target.ThreadID] = true
			address := member.Address
			if i > 0 {
				address = "history:" + member.Address + ":" + target.ThreadID
			}
			routes = append(routes, retainedObservationRoute{Address: address, Member: member.Address, Thread: target.ThreadID, Project: member.ProjectID, History: i > 0, Binding: resolved})
		}
	}
	return routes, nil
}

// ObserveRetained accepts an exact root already selected by its owning caller.
// It proves the current roster, project, persistence and unknown-writer state;
// it neither adopts a discovered root nor supplies creation provenance.
func (service Service) ObserveRetained(ctx context.Context, slug, rootThreadID, identity string) (Observation, error) {
	return service.observeRetained(ctx, slug, rootThreadID, identity, nil)
}

func (service Service) ObserveRetainedExceptPreparedRoot(ctx context.Context, slug, rootThreadID, identity string, prepared workspacecodex.PreparedSend) (Observation, error) {
	return service.observeRetained(ctx, slug, rootThreadID, identity, &prepared)
}

func (service Service) observeRetained(ctx context.Context, slug, rootThreadID, identity string, prepared *workspacecodex.PreparedSend) (Observation, error) {
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
		teamIdentities := append([][3]string{}, identities...)
		cwd := filepath.Join(service.Store.workspace, "work", slug)
		routes, err := service.Store.retainedObservationRoutes(slug, rootThreadID, roster)
		if err != nil {
			return err
		}
		active := map[string]string{}
		states := map[string]workspacecodex.ArchiveState{}
		routeState := func(route retainedObservationRoute) (workspacecodex.ArchiveState, error) {
			if route.Member == "lead" {
				return observer.ProveArchivedRootThread(ctx, route.Thread, cwd)
			}
			if !route.History {
				for _, member := range roster.Members {
					if member.Address == route.Member {
						member.Thread = route.Thread
						archived, err := service.retainedMemberArchiveState(ctx, member, cwd)
						if err != nil {
							return workspacecodex.ArchiveUnknown, err
						}
						if archived {
							return workspacecodex.ArchiveArchived, nil
						}
						return workspacecodex.ArchiveActive, nil
					}
				}
				return workspacecodex.ArchiveUnknown, errors.New("retained current member disappeared")
			}
			return service.Client.ProveArchivedThread(ctx, route.Thread, cwd, route.Project)
		}
		add := func(address, thread, project string, state workspacecodex.ArchiveState) error {
			archiveState := ""
			switch state {
			case workspacecodex.ArchiveActive:
				archiveState = "active"
			case workspacecodex.ArchiveFresh:
				archiveState = "fresh"
			case workspacecodex.ArchiveArchived:
				archiveState = "archived"
			default:
				return errors.New("retained archival state is unknown")
			}
			var observation workspacecodex.ThreadObservation
			var err error
			if address == "lead" && prepared != nil {
				exact, ok := service.Client.(interface {
					ObserveThreadIdentityExceptPreparedSend(context.Context, string, string, string, workspacecodex.ArchiveState, workspacecodex.PreparedSend) (workspacecodex.ThreadObservation, error)
				})
				if !ok {
					return errors.New("semantic conversation observer is unavailable")
				}
				observation, err = exact.ObserveThreadIdentityExceptPreparedSend(ctx, thread, cwd, project, state, *prepared)
			} else {
				observation, err = observer.ObserveThreadIdentity(ctx, thread, cwd, project, state)
			}
			if err != nil {
				return err
			}
			if observation.Schema != 1 || observation.ThreadID != thread || observation.Cwd != cwd || !observation.ActivityKnown || observation.ActivityToken == "" {
				return errors.New("retained observation returned invalid subject evidence")
			}
			result.Subjects = append(result.Subjects, ObservationSubject{Address: address, ThreadID: thread, ProjectID: project,
				ActivityToken: observation.ActivityToken, LastActivityAt: observation.LastActivityAt, Idle: observation.Idle, ArchiveState: archiveState})
			result.Idle = result.Idle && observation.Idle
			result.Diagnostics = append(result.Diagnostics, observation.Diagnostics...)
			return nil
		}
		currentRoot := rootThreadID
		for _, route := range routes {
			state, err := routeState(route)
			if err != nil {
				return err
			}
			if err := add(route.Address, route.Thread, route.Project, state); err != nil {
				return err
			}
			states[route.Thread] = state
			if state != workspacecodex.ArchiveArchived {
				active[route.Thread] = route.Project
			}
			if route.Member == "lead" && !route.History {
				currentRoot = route.Thread
			}
			if route.History {
				identities = append(identities, [3]string{route.Address, route.Thread, route.Project})
			}
		}
		monitor, err := service.monitorLifecycle()
		if err != nil {
			return err
		}
		monitorInventory, err := monitor.Store.Inventory(ctx, slug, rootThreadID)
		if err != nil {
			return err
		}
		if len(monitorInventory.Unresolved) != 0 {
			return errors.New("monitor launch, submission or retirement cannot be verified")
		}
		for _, thread := range monitorInventory.Threads {
			op, err := monitor.Store.Get(ctx, thread.OperationID)
			if err != nil {
				return err
			}
			state, err := monitor.State(ctx, op)
			if err != nil {
				return err
			}
			address := "monitor:" + thread.OperationID
			if err := add(address, thread.ThreadID, thread.ProjectID, state); err != nil {
				return err
			}
			identities = append(identities, [3]string{address, thread.ThreadID, thread.ProjectID})
			if state != workspacecodex.ArchiveArchived {
				if _, exists := active[thread.ThreadID]; exists {
					return errors.New("monitor thread duplicates retained ownership")
				}
				active[thread.ThreadID] = thread.ProjectID
			}
			if monitoroperation.HasUnresolvedEffects(op) {
				result.Idle = false
			}
		}
		if err := workspacecodex.RequireExactActiveConversations(ctx, service.Client, cwd, currentRoot, active); err != nil {
			return err
		}
		monitorAgain, err := monitor.Store.Inventory(ctx, slug, rootThreadID)
		if err != nil || !reflect.DeepEqual(monitorInventory, monitorAgain) {
			return errors.New("monitor ownership changed during observation")
		}
		after, err := load()
		if err != nil || !reflect.DeepEqual(teamIdentities, retainedObservationIdentities(after)) {
			return errors.New("retained team identity changed during observation")
		}
		again, err := service.Store.retainedObservationRoutes(slug, rootThreadID, after)
		if err != nil || !reflect.DeepEqual(routes, again) {
			return errors.New("retained binding changed during observation")
		}
		for _, route := range routes {
			state, err := routeState(route)
			if err != nil {
				return err
			}
			if state != states[route.Thread] {
				return errors.New("retained archival state changed during observation")
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
		result.ActivityToken = retainedActivityToken(identities, result.Subjects)
		return nil
	})
	if err != nil {
		code := "retained_set_unverified"
		var transport *codex.TransportError
		if errors.As(err, &transport) {
			code = "transport_unavailable"
		}
		var failure *workspacecodex.ObservationError
		if errors.As(err, &failure) {
			code = failure.Code
		}
		result.Unknown(code, "Exact retained conversation activity cannot be verified.")
	}
	return result, nil
}

// Archival state is separate identity evidence. Preserve the existing
// semantic payload so adding this evidence never restarts ordinary grace.
func retainedActivityToken(identities [][3]string, subjects []ObservationSubject) string {
	activity := append([]ObservationSubject(nil), subjects...)
	for i := range activity {
		activity[i].ArchiveState = ""
	}
	return workspacecodex.ObservationToken([]any{1, identities, activity})
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
