package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

// A settled token only avoids unchanged scheduling work. It never authorizes a
// send; every changed, due or uncertain candidate retains full admission.
type retryIdleToken struct {
	identity, state, policy [32]byte
	observation             turnretry.Observation
	socket                  string
}

type retryIdleGate struct {
	mu     sync.Mutex
	tokens map[string]retryIdleToken
	live   map[string]bool
}

func retryDigest(value any) [32]byte {
	data, _ := json.Marshal(value) // All callers use validated JSON value types.
	return sha256.Sum256(data)
}

func (g *retryIdleGate) matches(id string, token retryIdleToken) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	previous, found := g.tokens[id]
	return found && previous == token
}

func (g *retryIdleGate) replace(id string, token *retryIdleToken) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tokens, id)
	if token != nil && (g.live == nil || g.live[id]) {
		if g.tokens == nil {
			g.tokens = make(map[string]retryIdleToken)
		}
		g.tokens[id] = *token
	}
}

func (g *retryIdleGate) prune(ids []string) {
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.live = live
	for id := range g.tokens {
		if !live[id] {
			delete(g.tokens, id)
		}
	}
}

func retryCanSettle(state turnretry.State, now time.Time) bool {
	return state.Enrolled && !state.Uncertain() &&
		!(state.Phase == "scheduled" && state.Pending != nil && max(now.UnixMilli(), state.ClockMS) >= state.DeadlineMS)
}

func retryPolicyDigest(policy turnretry.Policy, thread string) ([32]byte, bool) {
	entry := policy.Thread(thread)
	if entry == nil {
		return [32]byte{}, false
	}
	return retryDigest(struct {
		Scope                   turnretry.PolicyScope
		Revision                string
		Capacity, Cybersecurity bool
		Thread                  turnretry.ThreadPolicy
	}{policy.Scope, policy.Revision, policy.Capacity, policy.Cybersecurity, *entry}), true
}

// The checkpoint wrapper captures only values actually processed under Step's
// admission fence. An unlocked post-Step load could certify concurrent manual
// input that Step never observed. A staged token is published only on success.
type retrySettlement struct {
	turnretry.SessionPolicy
	identity    [32]byte
	observation *turnretry.Observation
	token       *retryIdleToken
	finalized   bool
	socket      string
}

func (p *retrySettlement) Checkpoint(ctx context.Context, state *turnretry.State, observation *turnretry.Observation, now time.Time, fresh bool) error {
	p.token = nil
	if err := p.SessionPolicy.Checkpoint(ctx, state, observation, now, fresh); err != nil {
		return err
	}
	if p.finalized {
		return nil // A later dispatch/receipt checkpoint invalidates settlement.
	}
	if observation != nil {
		value := *observation
		p.observation = &value
		return nil
	}
	if p.observation == nil {
		return nil
	}
	p.finalized = true
	if !retryCanSettle(*state, now) {
		return nil
	}
	policy, err := p.Store.Load(p.Scope)
	if err != nil {
		return nil // Failed optional capture cannot change the completed Step.
	}
	digest, complete := retryPolicyDigest(policy, state.Scope.Thread)
	socket, socketErr := session.SocketIdentity(state.Scope.Socket)
	if complete && socketErr == nil && socket == p.socket {
		p.token = &retryIdleToken{p.identity, retryDigest(*state), digest, *p.observation, socket}
	}
	return nil
}

// Passive reads confer no authority and perform no tmux or mutation admission.
// Missing enrollment/policy is handled by the existing full path, not as idle.
func (s *Server) retryIdleCandidate(ctx context.Context, client turnRetryClient, id string, now time.Time) (retryIdleToken, bool, error) {
	var empty retryIdleToken
	slug, address, valid := parseTeamConversationID(id)
	if !valid {
		return empty, false, nil
	}
	if err := s.requireCurrentHostProfile(); err != nil {
		return empty, false, nil
	}
	if err := s.requireProviderAdmission(slug); err != nil {
		return empty, false, nil
	}
	summary, err := session.Find(s.config.Workspace, slug)
	if err != nil {
		return empty, false, nil
	}
	if summary.Archived || summary.Lifecycle != "active" || session.RequireRetainedReady(summary, s.config.CodexSocket) != nil {
		return empty, false, nil
	}
	if receipt, ok := s.currentCreation(slug); ok && receipt.blocksSession() {
		return empty, false, nil
	}
	if err := RequireObservationReceipts(summary, s.config.UserStateRoot, "", ""); err != nil {
		return empty, false, nil
	}
	authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != summary.Codex.ThreadID || authority.CodexSocketPath != summary.Codex.SocketPath || authority.CodexClientVersion != summary.Codex.ClientVersion {
		return empty, false, nil
	}
	roster, err := s.loadTeamRoster(summary)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	thread := summary.Codex.ThreadID
	if address != "" {
		thread = ""
		if roster != nil {
			for _, member := range roster.Members {
				if member.Address == address && member.State == "ready" && member.RetireIntent == "" && member.RemovedAt == nil {
					thread = member.Thread
				}
			}
		}
		if thread == "" {
			return empty, false, nil
		}
	}
	scope := turnretry.Scope{Workspace: s.config.Workspace, Slug: slug, Root: summary.Codex.ThreadID, Thread: thread, Socket: s.config.CodexSocket}
	actual, err := client.RetryScope(ctx, thread)
	if err != nil || actual != scope {
		return empty, false, nil
	}
	identity, err := policyScope(summary)
	if err != nil {
		return empty, false, nil
	}
	state, err := client.RetryStore().Load(scope)
	if errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	if !retryCanSettle(state, now) || state.Boundary != identity.Incarnation {
		return empty, false, nil
	}
	policy, err := policyStore(client).Load(identity)
	if errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	policyDigest, complete := retryPolicyDigest(policy, thread)
	if !complete {
		return empty, false, nil
	}
	observation, err := client.RetryBackend().Observe(ctx, scope)
	if err != nil {
		return empty, false, err
	}
	socket, err := session.SocketIdentity(scope.Socket)
	if err != nil {
		return empty, false, nil // Unavailable optimization proof is full fallback.
	}
	identityDigest := retryDigest(struct {
		Scope     turnretry.Scope
		Policy    turnretry.PolicyScope
		Authority session.RuntimeAuthority
		Roster    any
	}{scope, identity, authority, roster})
	return retryIdleToken{identityDigest, retryDigest(state), policyDigest, observation, socket}, true, nil
}

func (s *Server) stepTurnRetrySettled(ctx context.Context, client turnRetryClient, id string, gate *retryIdleGate, now time.Time) error {
	candidate, complete, err := s.retryIdleCandidate(ctx, client, id, now)
	if err == nil && complete && gate.matches(id, candidate) {
		return nil
	}
	gate.replace(id, nil)
	if err != nil {
		return err
	}
	settlement := &retrySettlement{identity: candidate.identity, socket: candidate.socket}
	err = s.stepTurnRetryCapturing(ctx, client, id, settlement)
	if err == nil && complete {
		gate.replace(id, settlement.token)
	}
	return err
}
