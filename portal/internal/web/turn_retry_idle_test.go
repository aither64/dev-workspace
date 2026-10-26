package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type idleRetryBackend struct {
	scopes       map[string]turnretry.Scope
	boundaries   map[string]string
	observations map[string]turnretry.Observation
	reads, sends int
	unknown      bool
	receipt      string
	err          error
}

func (b *idleRetryBackend) Observe(_ context.Context, scope turnretry.Scope) (turnretry.Observation, error) {
	b.reads++
	return b.observations[scope.Thread], b.err
}
func (b *idleRetryBackend) Boundary(_ context.Context, scope turnretry.Scope) (string, error) {
	return b.boundaries[scope.Slug], nil
}
func (b *idleRetryBackend) Prove(_ context.Context, _ turnretry.Scope, state *turnretry.State, turn string) error {
	p := turnretry.Snapshot{Epoch: state.Epoch, Version: 1, ID: "managed-" + turn, Turn: turn, Origin: "browser", Provider: "fixture", Mode: "default", Options: codex.TurnOptions{Model: "fixture", ReasoningEffort: "high"}, Proven: true}
	p.Digest, _ = p.Hash()
	return state.AddSnapshot(p)
}
func (*idleRetryBackend) Check(context.Context, turnretry.Scope, turnretry.Snapshot) error {
	return nil
}
func (b *idleRetryBackend) Reconcile(context.Context, turnretry.Scope, turnretry.Pending) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{TurnID: b.receipt}, b.receipt != "", nil
}
func (b *idleRetryBackend) NeverSubmitted(context.Context, turnretry.Scope, turnretry.Pending) (bool, error) {
	return !b.unknown || b.sends == 0, nil
}
func (b *idleRetryBackend) Continue(_ context.Context, scope turnretry.Scope, _ turnretry.Pending) (codex.SendReceipt, error) {
	b.sends++
	if b.unknown {
		return codex.SendReceipt{}, &codex.UnknownSendOutcomeError{Err: errors.New("synthetic lost reply")}
	}
	b.receipt = "accepted"
	b.observations[scope.Thread] = turnretry.Observation{Turn: b.receipt, Status: "inProgress"}
	return codex.SendReceipt{TurnID: b.receipt}, nil
}

type idleRetryClient struct {
	*workspacecodex.Client
	store   turnretry.Store
	backend *idleRetryBackend
}

func (*idleRetryClient) RetryAvailable() bool              { return true }
func (c *idleRetryClient) RetryStore() turnretry.Store     { return c.store }
func (c *idleRetryClient) RetryBackend() turnretry.Backend { return c.backend }
func (c *idleRetryClient) RetryScope(_ context.Context, thread string) (turnretry.Scope, error) {
	scope, ok := c.backend.scopes[thread]
	if !ok {
		return scope, errors.New("unknown synthetic thread")
	}
	return scope, nil
}
func (*idleRetryClient) ProveArchivedThread(context.Context, string, string, string) (workspacecodex.ArchiveState, error) {
	return workspacecodex.ArchiveActive, nil
}

type idleRetryFixture struct {
	s       *Server
	client  *idleRetryClient
	gate    *retryIdleGate
	ids     []string
	tmuxLog string
	checks  int
	socket  string
}

func newIdleRetryFixture(t *testing.T, roots, members int) *idleRetryFixture {
	t.Helper()
	s := newTestServer(t)
	socketDir, err := os.MkdirTemp("/tmp", "retry-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	s.config.CodexSocket = filepath.Join(socketDir, "socket")
	listener, err := net.Listen("unix", s.config.CodexSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	b := &idleRetryBackend{scopes: map[string]turnretry.Scope{}, boundaries: map[string]string{}, observations: map[string]turnretry.Observation{}}
	c := &idleRetryClient{store: turnretry.Store{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}, backend: b}
	f := &idleRetryFixture{s: s, client: c, gate: &retryIdleGate{}, tmuxLog: filepath.Join(t.TempDir(), "tmux.log"), socket: s.config.CodexSocket}
	s.config.Codex = c
	lines := t.TempDir()
	for root := 0; root < roots; root++ {
		slug := fmt.Sprintf("idle%d", root)
		prepareInteractiveConversation(t, s, slug)
		rootThread := "root-" + slug
		manifest := filepath.Join(s.config.Workspace, "work", slug, "portal.yml")
		data, _ := os.ReadFile(manifest)
		content := strings.ReplaceAll(strings.ReplaceAll(string(data), "thread-1", rootThread), "/run/dev-workspace-codex/app-server.sock", s.config.CodexSocket)
		if err := os.WriteFile(manifest, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		authority, err := session.LoadRuntimeAuthority(s.config.AuthorityDir, slug, s.config.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		authority.CodexThreadID, authority.TmuxSessionID = rootThread, fmt.Sprintf("$%d", root+1)
		data, _ = json.Marshal(authority)
		if err := os.WriteFile(filepath.Join(s.config.AuthorityDir, slug+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		line := strings.Join([]string{authority.TmuxSessionID, slug, "1", slug, s.config.Workspace, slug, authority.TmuxSocket, rootThread, authority.CodexSocketPath, authority.CodexClientVersion, "%1", authority.TmuxIdentity}, "\t")
		if err := os.WriteFile(filepath.Join(lines, authority.TmuxSessionID), []byte(line+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		summary, _ := session.Find(s.config.Workspace, slug)
		identity, _ := policyScope(summary)
		b.boundaries[slug] = identity.Incarnation
		threads := []string{rootThread}
		f.ids = append(f.ids, slug)
		store, _ := teamruntime.NewStore(s.config.UserStateRoot, s.config.Workspace)
		if _, err := store.Update(context.Background(), slug, rootThread, true, func(roster *teamruntime.Roster) error {
			for member := 0; member < members; member++ {
				address := fmt.Sprintf("implementer%d", member)
				thread := slug + "-" + address
				threads = append(threads, thread)
				f.ids = append(f.ids, teamConversationID(slug, address))
				roster.Members = append(roster.Members, teamruntime.Member{Address: address, Role: "implementer", Index: uint64(member), Thread: thread, Model: "fixture", Effort: "high", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		policyThreads := []turnretry.PolicyThread{}
		for _, thread := range threads {
			scope := turnretry.Scope{Workspace: s.config.Workspace, Slug: slug, Root: rootThread, Thread: thread, Socket: s.config.CodexSocket}
			b.scopes[thread] = scope
			tx, err := c.store.Lock(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			tx.State.Enroll(turnretry.Observation{})
			tx.State.Boundary = identity.Incarnation
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			policyThreads = append(policyThreads, turnretry.PolicyThread{State: tx.State})
			tx.Close()
		}
		if _, err := policyStore(c).Update(context.Background(), identity, true, func(p *turnretry.Policy) error {
			return turnretry.PreparePolicy(p, policyThreads, true, true, "", true)
		}); err != nil {
			t.Fatal(err)
		}
	}
	tmux := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf 'check\\n' >> '"+f.tmuxLog+"'\ncat '"+lines+"/'\"${6%:}\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	s.config.Tmux = tmux
	s.config.VerifyThread = func(_ context.Context, thread, cwd string) error {
		f.checks++
		scope, ok := b.scopes[thread]
		if !ok || cwd != filepath.Join(s.config.Workspace, "work", scope.Slug) {
			return errors.New("synthetic thread changed")
		}
		return nil
	}
	f.gate.prune(f.ids)
	return f
}

func (f *idleRetryFixture) step(t *testing.T, id string, now time.Time) {
	t.Helper()
	if err := f.s.stepTurnRetrySettled(context.Background(), f.client, id, f.gate, now); err != nil {
		t.Fatal(err)
	}
}

func (f *idleRetryFixture) scope(id string) turnretry.Scope {
	slug, address, _ := parseTeamConversationID(id)
	thread := "root-" + slug
	if address != "" {
		thread = slug + "-" + address
	}
	return f.client.backend.scopes[thread]
}

func TestSettledRetryIdleScaleKeepsPassiveReadsWithoutAdmission(t *testing.T) {
	f := newIdleRetryFixture(t, 20, 5)
	now := time.Now()
	for _, id := range f.ids {
		f.step(t, id, now)
	}
	if len(f.gate.tokens) != 120 || f.checks == 0 {
		t.Fatal("full initial settlement was not exercised", len(f.gate.tokens), f.checks)
	}
	checks, reads := f.checks, f.client.backend.reads
	tmuxBefore, _ := os.ReadFile(f.tmuxLog)
	for second := 1; second <= 60; second++ {
		for _, id := range f.ids {
			f.step(t, id, now.Add(time.Duration(second)*time.Second))
		}
	}
	tmuxAfter, _ := os.ReadFile(f.tmuxLog)
	if f.checks != checks || string(tmuxAfter) != string(tmuxBefore) || f.client.backend.reads-reads != 7200 || f.client.backend.sends != 0 {
		t.Fatal("unchanged idle work entered admission or stopped observation", f.checks-checks, f.client.backend.reads-reads, f.client.backend.sends)
	}
	f.gate.prune(nil)
	f.gate.replace(f.ids[0], &retryIdleToken{})
	if len(f.gate.tokens) != 0 {
		t.Fatal("retired worker repopulated tokens")
	}
}

func TestSettledRetryFailureDeadlineAndAcceptedCheckpoint(t *testing.T) {
	f := newIdleRetryFixture(t, 1, 0)
	id, now := f.ids[0], time.Now()
	scope := f.scope(id)
	f.step(t, id, now)
	f.client.backend.observations[scope.Thread] = turnretry.Observation{Turn: "failure", Status: "failed", Code: "serverOverloaded"}
	f.step(t, id, now)
	state, _ := f.client.store.Load(scope)
	if state.Phase != "scheduled" || f.client.backend.sends != 0 {
		t.Fatal("new failure was missed or dispatched early", state.Phase)
	}
	checks := f.checks
	f.step(t, id, now.Add(time.Second))
	if f.checks != checks {
		t.Fatal("unchanged future deadline entered admission")
	}
	candidate, complete, err := f.s.retryIdleCandidate(context.Background(), f.client, id, now.Add(time.Second))
	if err != nil || !complete || !f.gate.matches(id, candidate) {
		t.Fatal("future deadline did not have a matching settled token", err)
	}
	// Keep persisted state, policy and observation unchanged; only probe time
	// crosses the deadline. Digest invalidation must not be needed for admission.
	f.step(t, id, time.UnixMilli(state.DeadlineMS).Add(time.Millisecond))
	if f.checks == checks {
		t.Fatal("unchanged settled deadline did not resume full admission")
	}
	// Advancing the durable due clock exercises the real controller immediately.
	tx, _ := f.client.store.Lock(context.Background(), scope)
	tx.State.DeadlineMS = 1
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	f.step(t, id, now)
	if f.client.backend.sends != 1 || f.gate.matches(id, f.gate.tokens[id]) {
		t.Fatal("due attempt did not dispatch once or dispatch certified settlement")
	}
	// The next full pass processes the accepted result and its idempotent count.
	f.step(t, id, now)
	summary, _ := session.Find(f.s.config.Workspace, scope.Slug)
	identity, _ := policyScope(summary)
	p, _ := policyStore(f.client).Load(identity)
	entry := p.Thread(scope.Thread).Summary
	if entry == nil || entry.ConfirmedStarts == nil || *entry.ConfirmedStarts != 1 {
		t.Fatal("accepted count checkpoint was not settled", entry)
	}
	checks = f.checks
	f.step(t, id, now)
	if f.client.backend.sends != 1 || f.checks != checks {
		t.Fatal("accepted unchanged result was repeated")
	}
	if _, err := policyStore(f.client).Update(context.Background(), identity, false, func(p *turnretry.Policy) error {
		v := p.Thread(scope.Thread).Summary
		*v.ConfirmedStarts, v.LastConfirmed = 0, ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.step(t, id, now)
	p, _ = policyStore(f.client).Load(identity)
	if *p.Thread(scope.Thread).Summary.ConfirmedStarts != 1 || f.client.backend.sends != 1 || f.checks == checks {
		t.Fatal("accepted checkpoint gap was treated as unchanged idle state")
	}
}

func TestSettledRetryMissingEnrollmentAndPolicyUseFullPath(t *testing.T) {
	for _, missing := range []string{"state", "enrollment", "policy", "thread"} {
		t.Run(missing, func(t *testing.T) {
			f := newIdleRetryFixture(t, 1, 0)
			id, now := f.ids[0], time.Now()
			scope := f.scope(id)
			summary, _ := session.Find(f.s.config.Workspace, scope.Slug)
			identity, _ := policyScope(summary)
			switch missing {
			case "state", "policy":
				pattern := filepath.Join(f.client.store.Root, "turn-retries", "*", scope.Slug, "*.json")
				if missing == "policy" {
					pattern = filepath.Join(f.client.store.Root, "turn-retry-policy", "*", scope.Slug+".json")
				}
				paths, _ := filepath.Glob(pattern)
				for _, path := range paths {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			case "enrollment":
				tx, _ := f.client.store.Lock(context.Background(), scope)
				tx.State.Enrolled = false
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
			case "thread":
				if _, err := policyStore(f.client).Update(context.Background(), identity, false, func(p *turnretry.Policy) error {
					p.Threads = []turnretry.ThreadPolicy{}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.step(t, id, now)
			state, err := f.client.store.Load(scope)
			p, policyErr := policyStore(f.client).Load(identity)
			if err != nil || policyErr != nil || !state.Enrolled || p.Thread(scope.Thread) == nil || len(f.gate.tokens) != 0 || f.checks == 0 || f.client.backend.sends != 0 {
				t.Fatal("incomplete input was cached idle or skipped bootstrap", err, policyErr)
			}
			f.step(t, id, now)
			if len(f.gate.tokens) != 1 {
				t.Fatal("completed bootstrap did not subsequently settle")
			}
		})
	}
}

func TestSettledRetryChangesInvalidateEvenWithoutNewPolicyRevision(t *testing.T) {
	for _, change := range []string{"manual", "policy", "checkpoint", "socket", "retirement", "generation", "malformed", "observation-error"} {
		t.Run(change, func(t *testing.T) {
			f := newIdleRetryFixture(t, 1, 1)
			id, now := f.ids[1], time.Now()
			scope := f.scope(id)
			f.step(t, id, now)
			checks := f.checks
			summary, _ := session.Find(f.s.config.Workspace, scope.Slug)
			identity, _ := policyScope(summary)
			policy, _ := policyStore(f.client).Load(identity)
			wantError := false
			switch change {
			case "manual":
				tx, _ := f.client.store.Lock(context.Background(), scope)
				tx.State.Cancel("manual_input")
				if err := tx.Save(); err != nil {
					t.Fatal(err)
				}
				tx.Close()
			case "policy", "checkpoint":
				_, err := policyStore(f.client).Update(context.Background(), identity, false, func(p *turnretry.Policy) error {
					if change == "policy" {
						p.Capacity = false // Deliberately retain the same Revision.
					} else {
						p.Thread(scope.Thread).Capacity = turnretry.Enrollment{Turn: "different"}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				current, _ := policyStore(f.client).Load(identity)
				if current.Revision != policy.Revision {
					t.Fatal("test changed the policy revision")
				}
			case "socket":
				if err := os.Rename(f.socket, f.socket+".old"); err != nil {
					t.Fatal(err)
				}
				listener, err := net.Listen("unix", f.socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			case "retirement":
				store, _ := teamruntime.NewStore(f.s.config.UserStateRoot, f.s.config.Workspace)
				if _, err := store.Update(context.Background(), scope.Slug, scope.Root, false, func(r *teamruntime.Roster) error {
					r.Members[0].State = "archived"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				wantError = true
			case "generation":
				if err := os.Remove(f.s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), f.s.config.HostProfile); err != nil {
					t.Fatal(err)
				}
				wantError = true
			case "malformed":
				paths, _ := filepath.Glob(filepath.Join(f.client.store.Root, "turn-retries", "*", scope.Slug, "*.json"))
				for _, path := range paths {
					if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				wantError = true
			case "observation-error":
				f.client.backend.err = errors.New("synthetic read failure")
				wantError = true
			}
			err := f.s.stepTurnRetrySettled(context.Background(), f.client, id, f.gate, now)
			if (err != nil) != wantError || f.client.backend.sends != 0 {
				t.Fatal("changed input was skipped or executed", err, f.client.backend.sends)
			}
			if !wantError && f.checks == checks {
				t.Fatal("changed input bypassed full admission")
			}
			if wantError && len(f.gate.tokens) != 0 {
				t.Fatal("failure retained a settled token")
			}
		})
	}
}

func TestSettledRetryUncertaintyReconcilesAfterDisableAndCancellation(t *testing.T) {
	f := newIdleRetryFixture(t, 1, 0)
	id, now := f.ids[0], time.Now()
	scope := f.scope(id)
	f.step(t, id, now)
	f.client.backend.observations[scope.Thread] = turnretry.Observation{Turn: "failure", Status: "failed", Code: "cyberPolicy"}
	f.client.backend.unknown = true
	dispatchErr := f.s.stepTurnRetrySettled(context.Background(), f.client, id, f.gate, now)
	if dispatchErr == nil {
		t.Fatal("unknown dispatch was hidden")
	}
	tx, _ := f.client.store.Lock(context.Background(), scope)
	if !tx.State.Uncertain() {
		t.Fatal("fixture did not leave uncertainty", dispatchErr, tx.State)
	}
	tx.State.Cancel("manual_input")
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	summary, _ := session.Find(f.s.config.Workspace, scope.Slug)
	identity, _ := policyScope(summary)
	if _, err := policyStore(f.client).Update(context.Background(), identity, false, func(p *turnretry.Policy) error {
		p.Capacity, p.Cybersecurity = false, false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.client.backend.receipt, f.client.backend.unknown = "accepted-later", false
	f.step(t, id, now)
	state, _ := f.client.store.Load(scope)
	if state.Uncertain() || !state.Cancelled || state.Pending == nil || state.Pending.ResultTurn != "accepted-later" || f.client.backend.sends != 1 {
		t.Fatal("disabled/cancelled uncertainty lost exact receipt or resent", state, f.client.backend.sends)
	}
}

func TestSettledRetryUnavailableExecutionProofStillReconcilesReceipt(t *testing.T) {
	for _, hold := range []string{"authority", "provider"} {
		t.Run(hold, func(t *testing.T) {
			f := newIdleRetryFixture(t, 1, 0)
			id, now := f.ids[0], time.Now()
			scope := f.scope(id)
			f.step(t, id, now)
			f.client.backend.observations[scope.Thread] = turnretry.Observation{Turn: "failure", Status: "failed", Code: "cyberPolicy"}
			f.client.backend.unknown = true
			if err := f.s.stepTurnRetrySettled(context.Background(), f.client, id, f.gate, now); err == nil {
				t.Fatal("fixture did not leave an unknown send")
			}
			f.client.backend.receipt, f.client.backend.unknown = "accepted-later", false
			if hold == "authority" {
				authority, _ := session.LoadRuntimeAuthority(f.s.config.AuthorityDir, scope.Slug, scope.Workspace)
				authority.State = "creating"
				data, _ := json.Marshal(authority)
				if err := os.WriteFile(filepath.Join(f.s.config.AuthorityDir, scope.Slug+".json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				stateRoot := f.s.config.UserStateRoot
				providerTestCatalog(t, f.s, "http://127.0.0.1:1")
				f.s.config.UserStateRoot = stateRoot
				path, _ := codexproviders.StatePath(stateRoot, f.s.config.WorkspaceName)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				state := codexproviders.State{Schema: 2, Workspace: f.s.config.WorkspaceName, Root: scope.Workspace, Socket: scope.Socket, Active: "pool", Operation: &codexproviders.Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "openai", CatalogDigest: strings.Repeat("a", 64), Phase: "applying", Strategy: "scoped-v1", Session: scope.Slug, Stage: "preparing", Sessions: []string{scope.Slug}, Roots: map[string]string{scope.Slug: scope.Root}}}
				data, _ := json.Marshal(state)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.stepTurnRetrySettled(context.Background(), f.client, id, f.gate, now); err == nil {
				t.Fatal("execution hold was bypassed")
			}
			state, err := f.client.store.Load(scope)
			if err != nil || state.Uncertain() || state.Pending == nil || state.Pending.ResultTurn != "accepted-later" || f.client.backend.sends != 1 || len(f.gate.tokens) != 0 {
				t.Fatal("skip prefilter blocked receipt settlement or authorized execution", state, err)
			}
		})
	}
}

func TestSettledRetryCaptureDoesNotCertifyLaterManualState(t *testing.T) {
	f := newIdleRetryFixture(t, 1, 0)
	id, now := f.ids[0], time.Now()
	candidate, complete, err := f.s.retryIdleCandidate(context.Background(), f.client, id, now)
	if err != nil || !complete {
		t.Fatal(err, complete)
	}
	settlement := &retrySettlement{identity: candidate.identity, socket: candidate.socket}
	if err := f.s.stepTurnRetryCapturing(context.Background(), f.client, id, settlement); err != nil || settlement.token == nil {
		t.Fatal("full step did not capture under its fence", err)
	}
	scope := f.scope(id)
	tx, _ := f.client.store.Lock(context.Background(), scope)
	tx.State.Cancel("manual_input")
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if settlement.token.state == retryDigest(tx.State) {
		t.Fatal("checkpoint token sampled unprocessed manual state")
	}
	tx.Close()
	f.gate.replace(id, settlement.token)
	checks := f.checks
	f.step(t, id, now)
	if f.checks == checks {
		t.Fatal("later manual mutation was certified as settled")
	}
}
