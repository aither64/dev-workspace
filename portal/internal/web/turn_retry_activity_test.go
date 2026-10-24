package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

func prepareHTTPRetry(t *testing.T, f *planDeliveryFixture, member bool) (turnretry.Scope, turnretry.PolicyScope, turnretry.Policy, turnretry.Pending, string) {
	t.Helper()
	ctx := context.Background()
	thread, id := "thread-1", "example"
	if member {
		addPolicyMember(t, f)
		thread, id = "member-thread", "example~implementer0"
	}
	p, err := f.s.transactRetryPolicy(ctx, f.client, "example", nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := session.Find(f.s.config.Workspace, "example")
	identity, _ := policyScope(summary)
	scope, err := f.client.RetryScope(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.client.RetryStore().Lock(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Boundary = identity.Incarnation
	tx.State.Enroll(turnretry.Observation{})
	snapshot := turnretry.Snapshot{Epoch: 1, Version: 1, ID: "managed", Turn: "failed", Proven: true, Provider: "fixture", Mode: "default", Options: codex.TurnOptions{Model: "gpt-6.1-sol", ReasoningEffort: "xhigh"}}
	snapshot.Digest, _ = snapshot.Hash()
	if err := tx.State.AddSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	d := turnretry.Decision{Revision: p.Revision, Capacity: true, Cybersecurity: true}
	now := time.Now()
	if err := tx.State.Observe(turnretry.Observation{Turn: "failed", Status: "failed", Code: "serverOverloaded"}, now, 0, d); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if err := (turnretry.SessionPolicy{Store: policyStore(f.client), Scope: identity}).Checkpoint(ctx, &tx.State, nil, now, true); err != nil {
		t.Fatal(err)
	}
	pending := *tx.State.Pending
	tx.Close()
	return scope, identity, p, pending, id
}

func retryActivityPath(id string) string {
	if id == "example" {
		return "/api/sessions/" + id
	}
	return "/codex/conversations/" + id
}

func readHTTPActivity(t *testing.T, f *planDeliveryFixture, id string) conversation.RetryActivityStatus {
	t.Helper()
	w := retryRequest(f.s, context.Background(), "GET", retryActivityPath(id)+"/retry/activity", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result conversation.RetryActivityStatus
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRetryActivityActualHTTPRootMemberActionsIdentityAndNoDispatch(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "member"}[member], func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			scope, identity, p, pending, id := prepareHTTPRetry(t, f, member)
			stateBefore, _ := f.client.RetryStore().Load(scope)
			status := readHTTPActivity(t, f, id)
			if status.Activity == nil || status.Activity.Phase != "scheduled" || *status.Activity.ConfirmedStarts != 0 {
				t.Fatal("scheduled summary unavailable", status)
			}
			stopToken, nowToken := status.Activity.StopToken, status.Activity.RetryNowToken
			for _, token := range []string{"foreign-token", turnretry.ActionToken(identity, "foreign", stateBefore.Chain, p.Revision, "stop", "")} {
				w := retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+token+`"}`)
				if w.Code != 409 {
					t.Fatal("changed identity accepted", w.Code, w.Body.String())
				}
			}
			for i := 0; i < 2; i++ {
				w := retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/now", `{"token":"`+nowToken+`"}`)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			state, _ := f.client.RetryStore().Load(scope)
			if state.Pending.ID != pending.ID || state.Attempt != stateBefore.Attempt || !reflect.DeepEqual(state.Cyber, stateBefore.Cyber) || state.DeadlineMS > time.Now().UnixMilli() {
				t.Fatal("RetryNow dispatched/reserved/changed identity", state)
			}
			// Same original attempt can progress without invalidating its comparison
			// identity; RetryNow remains a no-op and Stop stays chain-scoped.
			tx, _ := f.client.RetryStore().Lock(context.Background(), scope)
			if err := tx.State.Begin(time.Now(), turnretry.Decision{Revision: p.Revision, Capacity: true, Cybersecurity: true}); err != nil {
				t.Fatal(err)
			}
			if err := tx.State.Accepted("accepted", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			w := retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/now", `{"token":"`+nowToken+`"}`)
			if w.Code != 200 {
				t.Fatal("duplicate RetryNow through acceptance rejected", w.Code, w.Body.String())
			}
			tx, _ = f.client.RetryStore().Lock(context.Background(), scope)
			nextProof := snapshotForHTTPRetry("accepted")
			if err := tx.State.AddSnapshot(nextProof); err != nil {
				t.Fatal(err)
			}
			if err := tx.State.Observe(turnretry.Observation{Turn: "accepted", Status: "failed", Code: "serverOverloaded"}, time.Now(), 0, turnretry.Decision{Revision: p.Revision, Capacity: true, Cybersecurity: true}); err != nil {
				t.Fatal(err)
			}
			if tx.State.Pending.ID == pending.ID || tx.State.Chain != stateBefore.Chain {
				t.Fatal("fixture did not advance same chain")
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			w = retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/now", `{"token":"`+nowToken+`"}`)
			if w.Code != 409 {
				t.Fatal("old Pending token authorized new attempt", w.Code, w.Body.String())
			}
			for i := 0; i < 2; i++ {
				w = retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+stopToken+`"}`)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			state, _ = f.client.RetryStore().Load(scope)
			if !state.Cancelled || state.Chain != stateBefore.Chain || state.Handled != "accepted" {
				t.Fatal("Stop did not consume exact accepted result", state)
			}
			original, _ := json.Marshal(state)
			w = retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+stopToken+`"}`)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			after, _ := f.client.RetryStore().Load(scope)
			data, _ := json.Marshal(after)
			if !bytes.Equal(original, data) {
				t.Fatal("duplicate Stop rewrote tombstone")
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 0 || f.wire.updates != 0 || f.wire.resumes != 0 {
				t.Fatal("actions invoked native mutation")
			}
		})
	}
}

func snapshotForHTTPRetry(turn string) turnretry.Snapshot {
	snapshot := turnretry.Snapshot{Epoch: 1, Version: 1, ID: "managed-" + turn, Turn: turn, Proven: true, Provider: "fixture", Mode: "default", Options: codex.TurnOptions{Model: "gpt-6.1-sol", ReasoningEffort: "xhigh"}}
	snapshot.Digest, _ = snapshot.Hash()
	return snapshot
}

func TestRetryActivityStopAcknowledgesLedgerWithUnavailableSummary(t *testing.T) {
	f := newPlanDeliveryFixture(t)
	scope, _, _, _, id := prepareHTTPRetry(t, f, false)
	status := readHTTPActivity(t, f, id)
	path := retryPolicyPath(f)
	directory := filepath.Dir(path)
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(directory, 0700)
	w := retryRequest(f.s, context.Background(), "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+status.Activity.StopToken+`"}`)
	if w.Code != 200 {
		t.Fatal("optional summary failure prevented Stop ack", w.Code, w.Body.String())
	}
	state, _ := f.client.RetryStore().Load(scope)
	if !state.Cancelled {
		t.Fatal("Stop ack preceded authoritative write")
	}
}

func TestRetryActivityActualHTTPStopsOwnUnknownBeforeOrdinaryGate(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "member"}[member], func(t *testing.T) {
			s, client, wire, scope, identity, p, id := prepareNativeHTTPRetry(t, member)
			c, ctx, thread := client.Client, context.Background(), scope.Thread
			tx, err := c.RetryStore().Lock(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			d := turnretry.Decision{Revision: p.Revision, Capacity: true, Cybersecurity: true}
			if err := tx.State.Begin(now, d); err != nil {
				t.Fatal(err)
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			pending := *tx.State.Pending
			chain := tx.State.Chain
			tx.Close()
			_, err = c.Client.ContinueFailedTurn(ctx, thread, turnretry.Continuation, pending.ID, pending.Action, "failed", pending.Snapshot.Options)
			var unknown *codex.UnknownSendOutcomeError
			if !errors.As(err, &unknown) {
				t.Fatal("fixture did not create durable native unknown", err)
			}
			if target, err := s.resolveConversation(ctx, conversation.ResolveRequest{ID: id, Operation: "automatic-retry", Mutation: true}); err == nil {
				target.Release()
				t.Fatal("own unknown did not block ordinary dispatch")
			}
			token := turnretry.ActionToken(identity, thread, chain, p.Revision, "stop", "")
			w := retryRequest(s, ctx, "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+token+`"}`)
			if w.Code != 200 {
				t.Fatal("own unknown blocked durable Stop", w.Code, w.Body.String())
			}
			state, err := c.RetryStore().Load(scope)
			if err != nil || !state.Cancelled || !state.Uncertain() || state.Pending.ID != pending.ID || len(state.Cyber) != 1 {
				t.Fatal("Stop lost unknown identity/charge", state, err)
			}
			wire.mu.Lock()
			wire.expose = true
			wire.mu.Unlock()
			w = retryRequest(s, ctx, "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+token+`"}`)
			if w.Code != 200 {
				t.Fatal("stopped exact receipt not reconciled", w.Code, w.Body.String())
			}
			var status conversation.RetryActivityStatus
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Activity == nil || status.Activity.ConfirmedStarts == nil || *status.Activity.ConfirmedStarts != 1 || status.Activity.Unconfirmed != 0 || status.Activity.Outcome != "stopped" {
				t.Fatal("exact native receipt was not counted once", w.Body.String(), err)
			}
			state, err = c.RetryStore().Load(scope)
			if err != nil || !state.Cancelled || state.Pending == nil || state.Pending.ID != pending.ID || state.Pending.ResultTurn != "accepted-"+thread || state.Handled != "accepted-"+thread || len(state.Cyber) != 1 || !state.Cyber[0].Accepted {
				t.Fatal("exact stopped settlement changed tuple/quota", state, err)
			}
			wire.mu.Lock()
			defer wire.mu.Unlock()
			if wire.starts != 1 {
				t.Fatal("action repeated native submission", wire.starts)
			}
		})
	}
}

func TestRetryActivityViewCheckpointContinuity(t *testing.T) {
	for _, test := range []struct {
		name, phase, pending, result, checkpointPending, confirmed, quality string
		cancelled, gap                                                      bool
		unconfirmed                                                         int
	}{
		{name: "accepted before checkpoint", phase: "running", pending: "P", result: "accepted", checkpointPending: "P", quality: "exact", gap: true},
		{name: "Stop checkpoint failed", phase: "cancelled", pending: "P", result: "accepted", checkpointPending: "P", quality: "exact", cancelled: true, gap: true},
		{name: "manual retirement", phase: "cancelled", checkpointPending: "P", quality: "exact", cancelled: true, gap: true},
		{name: "replaced pending", phase: "scheduled", pending: "Q", checkpointPending: "P", quality: "exact", gap: true},
		{name: "accepted caught up", phase: "running", pending: "P", result: "accepted", checkpointPending: "P", confirmed: "P", quality: "exact"},
		{name: "retired caught up", phase: "cancelled", checkpointPending: "P", confirmed: "P", quality: "exact", cancelled: true},
		{name: "scheduled", phase: "scheduled", pending: "P", checkpointPending: "P", quality: "exact"},
		{name: "unknown", phase: "reconciling", pending: "P", checkpointPending: "P", quality: "exact", unconfirmed: 1},
		{name: "partial accepted", phase: "running", pending: "P", result: "accepted", checkpointPending: "P", quality: "partial"},
		{name: "unavailable accepted", phase: "running", pending: "P", result: "accepted", checkpointPending: "P", quality: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			count, started := int64(0), now.Add(-time.Minute).UnixMilli()
			if test.confirmed != "" {
				count = 1
			}
			checkpoint := &turnretry.Summary{Chain: "chain", Class: "capacity", Phase: "scheduled", ConfirmedStarts: &count, CountQuality: test.quality, StartedAtMS: &started, TimingQuality: "exact", PendingID: test.checkpointPending, LastConfirmed: test.confirmed}
			if test.quality == "unavailable" {
				checkpoint.ConfirmedStarts = nil
			}
			scope := turnretry.PolicyScope{Workspace: "/fixture", Slug: "example", Root: "root", Incarnation: "incarnation"}
			policy := turnretry.Policy{Revision: "revision", Capacity: true, Cybersecurity: true, Threads: []turnretry.ThreadPolicy{{Thread: "root", Summary: checkpoint}}}
			state := turnretry.State{Scope: turnretry.Scope{Thread: "root"}, Chain: "chain", Class: "capacity", Phase: test.phase, Cancelled: test.cancelled, DeadlineMS: now.Add(time.Minute).UnixMilli()}
			if test.pending != "" {
				state.Pending = &turnretry.Pending{ID: test.pending, ResultTurn: test.result}
			}
			before, _ := json.Marshal([]any{state, policy})
			view := retryActivityView(scope, policy, state, now).Activity
			if view == nil {
				t.Fatal("activity disappeared")
			}
			if test.gap {
				if view.CountQuality != "unavailable" || view.ConfirmedStarts != nil {
					t.Fatalf("stale checkpoint remained exact: %+v", view)
				}
			} else if view.CountQuality != checkpoint.CountQuality || !reflect.DeepEqual(view.ConfirmedStarts, checkpoint.ConfirmedStarts) {
				t.Fatalf("continuous/already degraded count changed: %+v", view)
			}
			// Count reconciliation changes no action, phase or timing identity.
			if view.Unconfirmed != test.unconfirmed || view.ChainKey != turnretry.ActionToken(scope, "root", "chain", "", "chain", "") {
				t.Fatal("count view changed uncertainty/chain", view)
			}
			if test.cancelled {
				if view.Phase != "terminal" || view.Outcome != "stopped" || view.StopToken != "" || view.RetryNowToken != "" || view.StartedAtMS != nil || view.ElapsedMS != nil || view.TimingQuality != "unavailable" {
					t.Fatal("terminal identity/timing changed", view)
				}
			} else {
				if view.Phase != test.phase || view.Outcome != "" || view.StopToken != turnretry.ActionToken(scope, "root", "chain", "revision", "stop", "") || view.StartedAtMS == nil || *view.StartedAtMS != started || view.ElapsedMS == nil || *view.ElapsedMS != 60000 || view.TimingQuality != "exact" {
					t.Fatal("active identity/timing changed", view)
				}
				if test.phase == "scheduled" && (view.RetryNowToken != turnretry.ActionToken(scope, "root", "chain", "revision", "now", test.pending) || view.NextAttemptAtMS == nil || *view.NextAttemptAtMS != state.DeadlineMS) {
					t.Fatal("scheduled action changed", view)
				}
			}
			after, _ := json.Marshal([]any{state, policy})
			if !bytes.Equal(before, after) {
				t.Fatal("read-only view mutated ledger or checkpoint")
			}
		})
	}
}

func prepareNativeHTTPRetry(t *testing.T, member bool) (*Server, *resolverRetryClient, *retryWire, turnretry.Scope, turnretry.PolicyScope, turnretry.Policy, string) {
	t.Helper()
	s := newTestServer(t)
	ctx := context.Background()
	cwd := filepath.Join(s.config.Workspace, "work", "example")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "retry-action-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "native.sock")
	s.config.CodexSocket = socket
	manifest := "schema: 1\nslug: example\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: thread-1\n  socket_path: " + socket + "\n  client_version: 0.152.1\n"
	if err := os.WriteFile(filepath.Join(cwd, "portal.yml"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, cwd, "active")
	writeWebRuntimeAuthority(t, s, "example")
	wire := &retryWire{cwd: cwd, items: map[string][]any{}, latest: map[string]string{}}
	wire.serve(t, socket)
	c := workspacecodex.NewWithOptions(socket, s.config.Workspace, codex.ClientOptions{SubmissionLedgerPath: filepath.Join(s.config.UserStateRoot, "native.json")})
	t.Cleanup(func() { c.Close() })
	c.RecoveryRoot = s.config.UserStateRoot
	c.RetryPolicyResolver = func(context.Context, string) (codex.ThreadPolicy, error) { return codex.ThreadPolicy{}, nil }
	client := &resolverRetryClient{c, cwd}
	s.config.Codex = client
	thread, id := "thread-1", "example"
	if member {
		service, err := s.teamService("example")
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Store.Update(ctx, "example", "thread-1", true, func(r *teamruntime.Roster) error {
			r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "member-thread", Model: "gpt-6.1-sol", Effort: "xhigh", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		thread, id = "member-thread", "example~implementer0"
	}
	p, err := s.transactRetryPolicy(ctx, client, "example", nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := session.Find(s.config.Workspace, "example")
	identity, _ := policyScope(summary)
	scope, err := c.RetryScope(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.RetryStore().Lock(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Boundary = identity.Incarnation
	tx.State.Enroll(turnretry.Observation{})
	proof := snapshotForHTTPRetry("failed")
	if err := tx.State.AddSnapshot(proof); err != nil {
		t.Fatal(err)
	}
	d := turnretry.Decision{Revision: p.Revision, Capacity: true, Cybersecurity: true}
	now := time.Now()
	if err := tx.State.Observe(turnretry.Observation{Turn: "failed", Status: "failed", Code: "cyberPolicy"}, now, 0, d); err != nil {
		t.Fatal(err)
	}
	a := turnretry.SessionPolicy{Store: policyStore(client), Scope: identity}
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	if err := a.Checkpoint(ctx, &tx.State, nil, now, true); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	return s, client, wire, scope, identity, p, id
}

func TestRetryActivityActualHTTPCheckpointGaps(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "member"}[member], func(t *testing.T) {
			s, client, wire, scope, identity, policy, id := prepareNativeHTTPRetry(t, member)
			ctx := context.Background()
			f := &planDeliveryFixture{s: s}
			initial := readHTTPActivity(t, f, id).Activity
			if initial == nil || initial.CountQuality != "exact" || initial.ConfirmedStarts == nil || *initial.ConfirmedStarts != 0 {
				t.Fatal("missing exact scheduled checkpoint", initial)
			}
			tx, err := client.RetryStore().Lock(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.State.Begin(time.Now(), turnretry.Decision{Revision: policy.Revision, Capacity: true, Cybersecurity: true}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			pending := *tx.State.Pending
			tx.Close()
			_, err = client.Client.ContinueFailedTurn(ctx, scope.Thread, turnretry.Continuation, pending.ID, pending.Action, pending.Failure, pending.Snapshot.Options)
			var unknown *codex.UnknownSendOutcomeError
			if !errors.As(err, &unknown) {
				t.Fatal("fixture did not create native acceptance with lost reply", err)
			}
			wire.mu.Lock()
			wire.expose = true
			wire.mu.Unlock()
			path := retryPolicyPath(f)
			originalSummary, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0700) })
			authority := turnretry.SessionPolicy{Store: policyStore(client), Scope: identity}
			controller := turnretry.Controller{Store: client.RetryStore(), Backend: client.RetryBackend(), Policy: authority}
			if err := controller.ReconcileOnly(ctx, scope); err != nil {
				t.Fatal("optional checkpoint blocked exact settlement", err)
			}
			accepted, err := client.RetryStore().Load(scope)
			if err != nil || accepted.Pending == nil || accepted.Pending.ID != pending.ID || accepted.Pending.ResultTurn != "accepted-"+scope.Thread || accepted.Uncertain() || len(accepted.Cyber) != 1 || !accepted.Cyber[0].Accepted {
				t.Fatal("native receipt/charge did not settle exactly", accepted, err)
			}
			if err := authority.Checkpoint(ctx, &accepted, nil, time.Now(), false); err == nil {
				t.Fatal("fixture did not fail optional summary write")
			}
			workspaceHash, threadHash := sha256.Sum256([]byte(scope.Workspace)), sha256.Sum256([]byte(scope.Thread))
			ledgerPath := filepath.Join(s.config.UserStateRoot, "turn-retries", hex.EncodeToString(workspaceHash[:]), scope.Slug, hex.EncodeToString(threadHash[:])+".json")
			read := func(phase, outcome string) {
				t.Helper()
				ledger, err := os.ReadFile(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				ledgerInfo, err := os.Stat(ledgerPath)
				if err != nil {
					t.Fatal(err)
				}
				summaryInfo, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					view := readHTTPActivity(t, f, id).Activity
					if view == nil || view.CountQuality != "unavailable" || view.ConfirmedStarts != nil || view.Unconfirmed != 0 || view.Phase != phase || view.Outcome != outcome || view.ChainKey != initial.ChainKey {
						t.Fatal("stale optional checkpoint presented as exact", view)
					}
					if phase == "running" && (view.StopToken != initial.StopToken || view.RetryNowToken != "") || phase == "terminal" && (view.StopToken != "" || view.RetryNowToken != "") {
						t.Fatal("count reconciliation changed action identity", view)
					}
				}
				for _, file := range []struct {
					path string
					data []byte
					info os.FileInfo
				}{{ledgerPath, ledger, ledgerInfo}, {path, originalSummary, summaryInfo}} {
					data, err := os.ReadFile(file.path)
					info, statErr := os.Stat(file.path)
					if err != nil || statErr != nil || !bytes.Equal(data, file.data) || !os.SameFile(info, file.info) {
						t.Fatal("GET changed ledger/checkpoint bytes or identity", file.path, err, statErr)
					}
				}
			}
			read("running", "")
			for i := 0; i < 2; i++ {
				w := retryRequest(s, ctx, "POST", retryActivityPath(id)+"/retry/stop", `{"token":"`+initial.StopToken+`"}`)
				var status conversation.RetryActivityStatus
				if err := json.Unmarshal(w.Body.Bytes(), &status); w.Code != 200 || err != nil || status.Activity == nil || status.Activity.CountQuality != "unavailable" || status.Activity.ConfirmedStarts != nil || status.Activity.Outcome != "stopped" {
					t.Fatal("failed summary prevented honest Stop acknowledgment", w.Code, w.Body.String(), err)
				}
			}
			stopped, err := client.RetryStore().Load(scope)
			if err != nil || !stopped.Cancelled || stopped.Chain != accepted.Chain || !reflect.DeepEqual(stopped.Pending, accepted.Pending) || !reflect.DeepEqual(stopped.Cyber, accepted.Cyber) || stopped.Handled != accepted.Pending.ResultTurn || stopped.Attempt != accepted.Attempt {
				t.Fatal("Stop changed receipt/charge identity", stopped, err)
			}
			read("terminal", "stopped")
			if err := controller.ReconcileOnly(ctx, scope); err != nil {
				t.Fatal("saved receipt recovery became unavailable", err)
			}
			tx, err = client.RetryStore().Lock(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			tx.State.Cancel("manual_input")
			if tx.State.Pending != nil || tx.State.Handled != accepted.Pending.ResultTurn || tx.State.Chain != accepted.Chain || !reflect.DeepEqual(tx.State.Cyber, accepted.Cyber) {
				t.Fatal("manual retirement lost exact settlement", tx.State)
			}
			if err := tx.Save(); err != nil {
				t.Fatal(err)
			}
			tx.Close()
			read("terminal", "superseded")
			wire.mu.Lock()
			defer wire.mu.Unlock()
			if wire.starts != 1 {
				t.Fatal("read/action/recovery dispatched a fresh attempt", wire.starts)
			}
		})
	}
}
