package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
)

// StartupSelection keeps the source policy distinct from a prepared target,
// including an enable against the same package. Missing legacy state is false.
type StartupSelection struct {
	Schema                 int            `json:"schema"`
	Profile                string         `json:"profile"`
	Enabled                bool           `json:"enabled"`
	TargetEnabled          bool           `json:"targetEnabled"`
	Pending                bool           `json:"pending"`
	OperationID            string         `json:"operationId"`
	CandidatePackage       string         `json:"candidatePackage"`
	SourceProfileToken     string         `json:"sourceProfileToken"`
	AuthorizedProfileToken string         `json:"authorizedProfileToken,omitempty"`
	LaunchGrants           []StartupGrant `json:"launchGrants,omitempty"`
}

func (s Store) Startup() (StartupSelection, error) {
	data, e := privateRead(filepath.Join(s.directory(), "startup.json"))
	if errors.Is(e, os.ErrNotExist) {
		return StartupSelection{Schema: 1, Profile: s.Profile}, nil
	}
	if e != nil {
		return StartupSelection{}, e
	}
	var state StartupSelection
	if e = decode(data, &state); e != nil {
		return state, e
	}
	if state.Schema != 1 || state.Profile != s.Profile || !uuid.MatchString(state.OperationID) || !filepath.IsAbs(state.CandidatePackage) || state.SourceProfileToken == "" || len(state.LaunchGrants) > 128 || !state.Pending && state.TargetEnabled != state.Enabled {
		return state, errors.New("invalid maintenance startup selection")
	}
	seen := map[string]bool{}
	for _, g := range state.LaunchGrants {
		if g.Unit == "" || seen[g.Unit] || !filepath.IsAbs(g.Workspace) || !filepath.IsAbs(g.Native) || g.ArgvDigest == "" || state.AuthorizedProfileToken == "" {
			return state, errors.New("invalid startup grant state")
		}
		if g.Sequence == 0 || g.Sequence > 128 || len(g.Attempts) > 128 {
			return state, errors.New("invalid startup attempt sequence")
		}
		for i, a := range g.Attempts {
			if a.Sequence == 0 || a.Sequence > g.Sequence || a.Invocation == "" || a.PID <= 0 || a.ProcessIdentity == "" || a.Boot == "" || a.Cgroup == "" || (a.Disposition != "consumed" && a.Disposition != "failed" && a.Disposition != "ready") || i > 0 && g.Attempts[i-1].Sequence >= a.Sequence {
				return state, errors.New("invalid consumed startup attempt")
			}
		}
		if g.Invocation != "" && (len(g.Attempts) == 0 || g.Attempts[len(g.Attempts)-1].Invocation != g.Invocation || g.Attempts[len(g.Attempts)-1].Sequence != g.Sequence) {
			return state, errors.New("startup invocation has no process evidence")
		}
		seen[g.Unit] = true
	}
	return state, nil
}

func (s Store) StageStartup(ctx context.Context, id, candidate, source string, target bool) error {
	return s.locked(ctx, func() error {
		state, e := s.Startup()
		if e != nil {
			return e
		}
		if !uuid.MatchString(id) || !filepath.IsAbs(candidate) || source == "" {
			return errors.New("invalid startup request identity")
		}
		if state.OperationID == id && !state.Pending {
			if state.CandidatePackage == candidate && state.SourceProfileToken == source && state.TargetEnabled == target {
				return nil
			}
			return errors.New("completed startup identity changed")
		}
		if state.Pending {
			if state.OperationID == id && state.CandidatePackage == candidate && state.SourceProfileToken == source && state.TargetEnabled == target {
				return nil
			}
			return errors.New("another startup transition is pending")
		}
		state.Schema = 1
		state.OperationID = id
		state.CandidatePackage = candidate
		state.SourceProfileToken = source
		state.TargetEnabled = target
		state.Pending = true
		state.AuthorizedProfileToken = ""
		state.LaunchGrants = nil
		return atomic(filepath.Join(s.directory(), "startup.json"), state)
	})
}

func (s Store) FinishStartup(ctx context.Context, id, candidate, profileToken string) error {
	return s.locked(ctx, func() error {
		state, e := s.Startup()
		if e != nil {
			return e
		}
		if profileToken == "" || state.AuthorizedProfileToken == "" || state.OperationID != id || state.CandidatePackage != candidate || state.AuthorizedProfileToken != profileToken {
			return errors.New("startup completion proof changed")
		}
		for _, g := range state.LaunchGrants {
			if g.Invocation == "" || len(g.Attempts) == 0 || g.Attempts[len(g.Attempts)-1].Disposition != "ready" {
				return errors.New("startup grant has not reached native launch")
			}
		}
		state.Enabled = state.TargetEnabled
		state.Pending = false
		return atomic(filepath.Join(s.directory(), "startup.json"), state)
	})
}

// StartupGrant is a one-use launch authority for a pending selected generation.
// A directly observed ready invocation can also settle an already running unit.
// Consumed attempts remain retained even if exec fails; observation never
// replaces them and a replacement invocation cannot silently replay a grant.
type StartupGrant struct {
	Unit       string           `json:"unit"`
	Workspace  string           `json:"workspace"`
	Native     string           `json:"native"`
	ArgvDigest string           `json:"argvDigest"`
	Invocation string           `json:"invocation,omitempty"`
	Sequence   uint64           `json:"sequence"`
	Attempts   []StartupAttempt `json:"attempts,omitempty"`
}

func (s Store) AuthorizeStartupGrants(ctx context.Context, id, candidate, token string, grants []StartupGrant) error {
	return s.locked(ctx, func() error {
		state, e := s.Startup()
		if e != nil {
			return e
		}
		if !state.Pending || state.OperationID != id || state.CandidatePackage != candidate || token == "" || len(grants) > 128 {
			return errors.New("startup grant identity changed")
		}
		if state.AuthorizedProfileToken != "" {
			if state.AuthorizedProfileToken != token || len(state.LaunchGrants) != len(grants) {
				return errors.New("startup grant generation changed")
			}
			for i, g := range grants {
				old := state.LaunchGrants[i]
				old.Invocation = ""
				old.Sequence = 0
				old.Attempts = nil
				g.Sequence = 0
				g.Attempts = nil
				if !sameStartupTuple(old, g) {
					return errors.New("startup grant changed")
				}
			}
			return nil
		}
		seen := map[string]bool{}
		for _, g := range grants {
			if g.Unit == "" || seen[g.Unit] || !filepath.IsAbs(g.Workspace) || !filepath.IsAbs(g.Native) || g.ArgvDigest == "" || g.Invocation != "" || g.Sequence != 0 || len(g.Attempts) != 0 {
				return errors.New("invalid startup grant")
			}
			seen[g.Unit] = true
		}
		state.AuthorizedProfileToken = token
		for i := range grants {
			grants[i].Sequence = 1
			grants[i].Attempts = nil
		}
		state.LaunchGrants = grants
		return atomic(filepath.Join(s.directory(), "startup.json"), state)
	})
}
func (s Store) ConsumeStartupGrant(ctx context.Context, id, candidate, token, invocation string, grant StartupGrant, proof ...StartupAttempt) (bool, error) {
	enabled := false
	e := s.locked(ctx, func() error {
		state, e := s.Startup()
		if e != nil {
			return e
		}
		enabled = state.Enabled
		if !state.Pending {
			return nil
		}
		if state.OperationID != id || state.CandidatePackage != candidate || state.AuthorizedProfileToken != token || token == "" || invocation == "" {
			return errors.New("pending startup has no owning grant")
		}
		for i, g := range state.LaunchGrants {
			if g.Unit != grant.Unit {
				continue
			}
			old := g
			old.Invocation = ""
			old.Sequence = 0
			old.Attempts = nil
			if !sameStartupTuple(old, grant) || g.Invocation != "" {
				return errors.New("startup grant changed or already consumed")
			}
			if len(proof) != 1 || proof[0].PID <= 0 || proof[0].ProcessIdentity == "" || proof[0].Boot == "" || proof[0].Cgroup == "" {
				return errors.New("startup process proof required")
			}
			attempt := proof[0]
			attempt.Sequence = g.Sequence
			attempt.Invocation = invocation
			attempt.Disposition = "consumed"
			if len(g.Attempts) >= 128 {
				return errors.New("startup attempt evidence bound reached")
			}
			state.LaunchGrants[i].Attempts = append(g.Attempts, attempt)
			state.LaunchGrants[i].Invocation = invocation
			enabled = state.TargetEnabled
			return atomic(filepath.Join(s.directory(), "startup.json"), state)
		}
		return errors.New("native unit has no startup grant")
	})
	return enabled, e
}

func sameStartupTuple(a, b StartupGrant) bool {
	return a.Unit == b.Unit && a.Workspace == b.Workspace && a.Native == b.Native && a.ArgvDigest == b.ArgvDigest
}

type StartupAttempt struct {
	Sequence         uint64 `json:"sequence"`
	Invocation       string `json:"invocation"`
	PID              int    `json:"pid"`
	ProcessIdentity  string `json:"processIdentity"`
	Boot             string `json:"boot"`
	Cgroup           string `json:"cgroup"`
	Disposition      string `json:"disposition"`
	NativeExecutable string `json:"nativeExecutable,omitempty"`
	SocketEpoch      string `json:"socketEpoch,omitempty"`
}

func (s Store) ReconcileStartupAttempt(ctx context.Context, id, candidate, token string, grant StartupGrant, observed StartupAttempt, disposition, native, epoch string) error {
	return s.locked(ctx, func() error {
		state, e := s.Startup()
		if e != nil {
			return e
		}
		if !state.Pending || state.OperationID != id || state.CandidatePackage != candidate || state.AuthorizedProfileToken != token {
			return errors.New("startup reconciliation tuple changed")
		}
		for i, g := range state.LaunchGrants {
			if g.Unit != grant.Unit {
				continue
			}
			if disposition == "failed" && sameStartupTuple(g, grant) && g.Sequence == grant.Sequence+1 && g.Invocation == "" && len(g.Attempts) > 0 {
				old := g.Attempts[len(g.Attempts)-1]
				old.Disposition = observed.Disposition
				if old == observed && g.Attempts[len(g.Attempts)-1].Disposition == "failed" {
					return nil
				}
			}
			if !sameStartupTuple(g, grant) || g.Sequence != grant.Sequence || g.Invocation != grant.Invocation || len(g.Attempts) == 0 {
				return errors.New("startup reconciliation attempt changed")
			}
			last := g.Attempts[len(g.Attempts)-1]
			if last != observed {
				return errors.New("startup reconciliation proof stale")
			}
			switch disposition {
			case "ready":
				if native == "" || epoch == "" {
					return errors.New("startup ready proof missing")
				}
				last.Disposition = "ready"
				last.NativeExecutable = native
				last.SocketEpoch = epoch
			case "failed":
				if last.Disposition == "ready" || last.SocketEpoch != "" {
					return errors.New("successful startup effects need reconciliation")
				}
				last.Disposition = "failed"
				if g.Sequence >= 128 {
					return errors.New("startup replacement bound reached")
				}
				state.LaunchGrants[i].Sequence++
				state.LaunchGrants[i].Invocation = ""
			default:
				return errors.New("invalid startup reconciliation")
			}
			state.LaunchGrants[i].Attempts[len(g.Attempts)-1] = last
			return atomic(filepath.Join(s.directory(), "startup.json"), state)
		}
		return errors.New("startup grant absent")
	})
}

// StartupOnly distinguishes an absent operation directory from an unreadable or
// partial journal. A missing operation.json is never enough to change its owner.
func (s Store) StartupOnly(id string) (bool, error) {
	if !uuid.MatchString(id) || !filepath.IsAbs(s.Root) {
		return false, errors.New("invalid startup operation identity")
	}
	if info, err := os.Lstat(filepath.Dir(s.Path(id))); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("startup operation directory is invalid")
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	parent, err := os.Lstat(s.directory())
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("startup authority directory unavailable")
	}
	active, activeErr := s.Active()
	if activeErr == nil && !active.Terminal() {
		return false, errors.New("startup-only authority conflicts with full operation")
	}
	if activeErr != nil && !errors.Is(activeErr, os.ErrNotExist) {
		return false, activeErr
	}
	return true, nil
}

// ObserveExistingStartupGrant retains a directly observed ready invocation. It
// consumes no launch event and cannot replace uncertain or failed attempts.
func (s Store) ObserveExistingStartupGrant(ctx context.Context, expected StartupSelection, grant StartupGrant, ready StartupAttempt) error {
	return s.locked(ctx, func() error {
		only, err := s.StartupOnly(expected.OperationID)
		if err != nil || !only {
			return errors.New("startup-only authority changed")
		}
		state, err := s.Startup()
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state, expected) {
			return errors.New("startup observation selection changed")
		}
		if ready.Invocation == "" || ready.PID <= 0 || ready.ProcessIdentity == "" || ready.Boot == "" || ready.Cgroup == "" || ready.NativeExecutable == "" || ready.SocketEpoch == "" || ready.Disposition != "ready" {
			return errors.New("startup existing ready proof missing")
		}
		for i, g := range state.LaunchGrants {
			if g.Unit != grant.Unit {
				continue
			}
			if !sameStartupTuple(g, grant) || g.Sequence != grant.Sequence || ready.Sequence != g.Sequence {
				return errors.New("startup observation grant changed")
			}
			if len(g.Attempts) > 0 && g.Invocation == ready.Invocation && g.Attempts[len(g.Attempts)-1] == ready {
				return nil
			}
			if !state.Pending || g.Sequence != 1 || g.Invocation != "" || len(g.Attempts) != 0 {
				return errors.New("startup existing observation would replace evidence")
			}
			state.LaunchGrants[i].Invocation = ready.Invocation
			state.LaunchGrants[i].Attempts = []StartupAttempt{ready}
			return atomic(filepath.Join(s.directory(), "startup.json"), state)
		}
		return errors.New("startup observation grant absent")
	})
}
