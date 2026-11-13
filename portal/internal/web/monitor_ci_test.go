package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"github.com/aither64/dev-workspace/portal/internal/processgroup"
)

func ciTestClaim(id string) monitoroperation.ObserverRecord {
	source := monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", RunID: "42", RunAttempt: 1, WorkflowID: "73", Commit: strings.Repeat("a", 40)}
	return monitoroperation.ObserverRecord{ID: id, Source: source, SourceDigest: monitoroperation.SourceDigest(source), Poll: &monitoroperation.PollReceipt{ID: strings.Repeat("b", 32), OwnerEpoch: strings.Repeat("c", 32), State: "intent", StartedAt: time.Now().UTC()}}
}
func ciTestExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func ciTestResponse(source monitoroperation.Source, status, conclusion string) []byte {
	data, _ := json.Marshal(map[string]any{"id": json.Number(source.RunID), "workflow_id": json.Number(source.WorkflowID), "run_attempt": source.RunAttempt, "head_sha": source.Commit, "repository": map[string]string{"full_name": source.Repository}, "status": status, "conclusion": conclusion})
	return data
}
func TestMonitorCIFixedReadAndLocalJoin(t *testing.T) {
	for _, test := range []struct {
		name, status, conclusion string
		exit                     int
	}{{"queued", "queued", "", 0}, {"running", "in_progress", "", 0}, {"terminal", "completed", "success", 0}, {"api failure", "queued", "", 17}} {
		t.Run(test.name, func(t *testing.T) {
			claim := ciTestClaim(strings.Repeat("1", 32))
			payload := string(ciTestResponse(claim.Source, test.status, test.conclusion))
			script := fmt.Sprintf("[ \"$#\" = 6 ] && [ \"$1\" = api ] && [ \"$2\" = --method ] && [ \"$3\" = GET ] && [ \"$4\" = --hostname ] && [ \"$5\" = github.com ] && [ \"$6\" = repos/example/project/actions/runs/42/attempts/1 ] || exit 91\nprintf '%%s' '%s'\nexit %d", payload, test.exit)
			publications := 0
			observer := ciPollObserver{controller: &ciPollController{}, claim: claim, executable: ciTestExecutable(t, script), publish: func(receipt monitoroperation.PollReceipt) error {
				publications++
				if receipt.State != "running" || receipt.Handle == "" || receipt.ID != claim.Poll.ID || receipt.OwnerEpoch != claim.Poll.OwnerEpoch {
					t.Error("unbound running receipt")
				}
				return nil
			}}
			snapshot, receipt, err := observer.Poll(context.Background(), claim.Source)
			if publications != 1 || receipt.State != "joined" || receipt.FinishedAt == nil || receipt.ExitStatus == nil || *receipt.ExitStatus != test.exit {
				t.Fatalf("receipt %+v publications%d error%v", receipt, publications, err)
			}
			if test.exit == 0 {
				if err != nil || snapshot.Status != test.status || snapshot.SourceDigest != claim.SourceDigest {
					t.Fatalf("snapshot %+v error%v", snapshot, err)
				}
			} else if err == nil || snapshot.Status != "" {
				t.Fatalf("API failure fabricated source completion: %+v %v", snapshot, err)
			}
		})
	}
}
func TestMonitorCISameSourceHasSeparateOwnedHandles(t *testing.T) {
	controller := &ciPollController{}
	if _, err := controller.Quiesce(context.Background(), "lost"); err == nil {
		t.Fatal("missing handle became proof")
	}
	executable := ciTestExecutable(t, "exec sleep 30")
	started := make(chan monitoroperation.PollReceipt, 2)
	result := make(chan error, 2)
	claims := []monitoroperation.ObserverRecord{ciTestClaim(strings.Repeat("1", 32)), ciTestClaim(strings.Repeat("2", 32))}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for _, claim := range claims {
		observer := &ciPollObserver{controller: controller, claim: claim, executable: executable, publish: func(receipt monitoroperation.PollReceipt) error { started <- receipt; return nil }}
		go func() {
			_, receipt, err := observer.Poll(ctx, observer.claim.Source)
			if receipt.State != "joined" {
				result <- fmt.Errorf("not joined: %+v %v", receipt, err)
				return
			}
			result <- nil
		}()
	}
	receipts := []monitoroperation.PollReceipt{}
	for range claims {
		select {
		case receipt := <-started:
			receipts = append(receipts, receipt)
		case <-ctx.Done():
			t.Fatal("requests did not start")
		}
	}
	if receipts[0].Handle == receipts[1].Handle {
		t.Fatal("same-source operations share a handle")
	}
	first, err := controller.Quiesce(ctx, claims[0].ID)
	if err != nil || first.State != "joined" {
		t.Fatal(first, err)
	}
	if !controller.OwnsPoll(claims[1].ID, *claims[1].Poll) {
		t.Fatal("quiescing one observer changed the other")
	}
	if _, err = controller.Quiesce(ctx, claims[1].ID); err != nil {
		t.Fatal(err)
	}
	for range claims {
		if err = <-result; err != nil {
			t.Fatal(err)
		}
	}
}
func TestMonitorCIRejectsSourceDriftAndIncompleteJoinProof(t *testing.T) {
	claim := ciTestClaim(strings.Repeat("1", 32))
	good := ciTestResponse(claim.Source, "queued", "")
	var response map[string]any
	for _, field := range []string{"id", "workflow_id", "run_attempt", "head_sha", "repository", "status", "conclusion"} {
		if err := json.Unmarshal(good, &response); err != nil {
			t.Fatal(err)
		}
		switch field {
		case "id", "workflow_id", "run_attempt":
			response[field] = 999
		case "repository":
			response[field] = map[string]string{"full_name": "foreign/repo"}
		default:
			response[field] = "different"
		}
		bad, _ := json.Marshal(response)
		if _, err := decodeCIRun(bad, claim.Source); err == nil {
			t.Fatalf("accepted changed %s", field)
		}
	}
	command := exec.Command("sh", "-c", "exit 3")
	proof, _ := processgroup.RunDetailed(context.Background(), command, nil)
	if !ciRequestJoined(proof, command) {
		t.Fatal("nonzero joined command rejected")
	}
	for _, change := range []func(*processgroup.Result){func(p *processgroup.Result) { p.WaitReturned = false }, func(p *processgroup.Result) { p.GroupAbsent = false }, func(p *processgroup.Result) { p.PipesComplete = false }, func(p *processgroup.Result) { p.PipeErr = exec.ErrWaitDelay }, func(p *processgroup.Result) { p.WaitErr = exec.ErrWaitDelay }, func(p *processgroup.Result) { p.CancelErr = errors.New("kill failed") }} {
		bad := proof
		change(&bad)
		if ciRequestJoined(bad, command) {
			t.Fatal("incomplete cleanup became joined")
		}
	}
}

func ciServiceFixture(t *testing.T, s *Server, owner monitoroperation.Owner) *monitoroperation.Service {
	t.Helper()
	store, err := monitoroperation.NewStore(s.config.UserStateRoot, s.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ciController()
	if err != nil {
		t.Fatal(err)
	}
	return &monitoroperation.Service{Store: store, ObserverEpoch: s.monitorCIEpoch,
		ResolveLead: func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil },
		Resolve:     func(string) (monitoroperation.Owner, error) { return owner, nil },
		LockSession: func(context.Context, string) (func(), error) { return func() {}, nil },
		Admit:       func(context.Context, monitoroperation.Owner) error { return nil },
		Policy: func(context.Context, monitoroperation.Owner) (monitoroperation.UtilityPolicy, error) {
			t.Fatal("native utility policy used for direct CI")
			return monitoroperation.UtilityPolicy{}, nil
		},
	}
}

func TestMonitorCIRegistrationRequiresActiveOriginatingTurn(t *testing.T) {
	for _, origin := range []string{"", "active-initial-turn"} {
		t.Run(fmt.Sprintf("origin=%q", origin), func(t *testing.T) {
			s := newTestServer(t)
			owner := monitoroperation.Owner{Slug: "monitor-test", RootThreadID: "lead", Incarnation: "fixture", OriginTurnID: origin, Socket: filepath.Join(s.config.Workspace, "native.sock"), SocketEpoch: "epoch", RuntimeSchema: 1}
			service := ciServiceFixture(t, s, owner)
			req := monitoroperation.StartRequest{ID: strings.Repeat("1", 32), Brief: "Observe the exact fixture run", Source: ciTestClaim(strings.Repeat("1", 32)).Source}
			result, err := service.StartManaged(context.Background(), owner.RootThreadID, req)
			if origin == "" {
				if err == nil || err.Error() != "CI observer lead changed" {
					t.Fatal("idle caller bypassed originating-turn admission", result, err)
				}
				if _, err = service.Store.GetObserver(context.Background(), req.ID); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("idle refusal created an observer", err)
				}
				return
			}
			ack, ok := result.(monitoroperation.ObserverStatus)
			if err != nil || !ok || ack.ExecutionOwner != "runtime-ci" || ack.Owner.OriginTurnID != origin {
				t.Fatal("active originating-turn binding not retained", result, err)
			}
			record, err := service.Store.GetObserver(context.Background(), req.ID)
			if err != nil || record.Owner.OriginTurnID != origin || record.Poll != nil || record.Remote != nil || len(record.Events) != 0 {
				t.Fatal("registration changed provenance or invented effects", record, err)
			}
			if _, err = service.Store.Get(context.Background(), req.ID); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("direct registration created native utility state", err)
			}
		})
	}
}

func TestMonitorCIWorkerQueuedArchiveAndLostOwner(t *testing.T) {
	s := newTestServer(t)
	claim := ciTestClaim(strings.Repeat("1", 32))
	owner := monitoroperation.Owner{Slug: "monitor-test", RootThreadID: "lead", Incarnation: "fixture", OriginTurnID: "origin", Socket: filepath.Join(s.config.Workspace, "native.sock"), SocketEpoch: "epoch", RuntimeSchema: 1}
	service := ciServiceFixture(t, s, owner)
	count := filepath.Join(t.TempDir(), "reads")
	payload := string(ciTestResponse(claim.Source, "queued", ""))
	s.config.GH = ciTestExecutable(t, fmt.Sprintf("printf x >> '%s'\nprintf '%%s' '%s'", count, payload))
	req := monitoroperation.StartRequest{ID: claim.ID, Brief: "Observe only the existing fixture run", Source: claim.Source}
	if _, err := service.StartManaged(context.Background(), owner.RootThreadID, req); err != nil {
		t.Fatal(err)
	}
	cursor := 0
	if err := s.runCIObservations(context.Background(), service, &cursor, time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	record, err := service.Store.GetObserver(context.Background(), claim.ID)
	if err != nil || record.Poll == nil || record.Poll.State != "joined" || record.Remote == nil || record.Remote.Status != "queued" || len(record.Events) != 0 {
		t.Fatalf("queued observation %+v %v", record, err)
	}
	if _, err = service.Store.Get(context.Background(), claim.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("direct observer created a native operation", err)
	}
	authority := monitoroperation.ArchiveAuthority{Slug: owner.Slug, RootThreadID: owner.RootThreadID, OperationID: strings.Repeat("d", 64), TargetID: strings.Repeat("e", 64), Mode: "archived"}
	lifecycle := monitoroperation.Lifecycle{Store: service.Store, Observer: s.monitorCI, AuthorizeArchive: func(_ context.Context, a monitoroperation.ArchiveAuthority) error {
		if a != authority {
			return errors.New("changed admission")
		}
		return nil
	}}
	if err = lifecycle.BeginArchiveQuiescence(context.Background(), authority); err != nil {
		t.Fatal(err)
	}
	local, err := lifecycle.CompleteArchiveQuiescence(context.Background(), authority)
	if err != nil || local.State != "quiescent" {
		t.Fatal(local, err)
	}
	if err = s.runCIObservations(context.Background(), service, &cursor, time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	record, err = service.Store.GetObserver(context.Background(), claim.ID)
	data, _ := os.ReadFile(count)
	if err != nil || record.Phase != "detached" || record.Remote.Status != "queued" || len(record.Events) != 0 || string(data) != "x" {
		t.Fatalf("detached observer polled/woke: %+v count%q err%v", record, data, err)
	}

	req.ID = strings.Repeat("2", 32)
	if _, err = service.StartManaged(context.Background(), owner.RootThreadID, req); err != nil {
		t.Fatal(err)
	}
	abandoned, err := service.ClaimCIPoll(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Server{config: s.config, hostProfile: s.hostProfile}
	_, err = restarted.ciController()
	if err != nil {
		t.Fatal(err)
	}
	service.ObserverEpoch = restarted.monitorCIEpoch
	if err = restarted.runCIObservations(context.Background(), service, &cursor, time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	after, err := service.Store.GetObserver(context.Background(), req.ID)
	if err != nil || after.Poll.ID != abandoned.Poll.ID || after.Poll.State != "intent" {
		t.Fatal("lost intent was relaunched/adopted", after, err)
	}
	local, err = (monitoroperation.Lifecycle{Store: service.Store, Observer: restarted.monitorCI}).AssessLocalSafety(context.Background(), owner.Slug, owner.RootThreadID)
	if err != nil || local.State != "blocked" {
		t.Fatal("lost owner became quiescent", local, err)
	}
	data, _ = os.ReadFile(count)
	if string(data) != "x" {
		t.Fatal("restart issued a duplicate read")
	}
}

func TestMonitorCIPreStartQuiescenceIsPositivelyUnsent(t *testing.T) {
	claim := ciTestClaim(strings.Repeat("1", 32))
	called := filepath.Join(t.TempDir(), "called")
	observer := ciPollObserver{controller: &ciPollController{}, claim: claim, executable: ciTestExecutable(t, "touch '"+called+"'"),
		admit:   func() error { return errors.New("archive intent is already quiescing") },
		publish: func(monitoroperation.PollReceipt) error { t.Error("published running before Start"); return nil },
	}
	_, receipt, err := observer.Poll(context.Background(), claim.Source)
	if err == nil || receipt.State != "joined" || receipt.FinishedAt == nil || receipt.ExitStatus != nil {
		t.Fatal("missing exact unsent proof", receipt, err)
	}
	if _, err = os.Stat(called); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("quiescing request was started", err)
	}
}

func TestMonitorCIWorkerPersistsKnownPreStartRefusal(t *testing.T) {
	for _, refusal := range []string{"arguments", "admission"} {
		t.Run(refusal, func(t *testing.T) {
			s := newTestServer(t)
			owner := monitoroperation.Owner{Slug: "monitor-test", RootThreadID: "lead", Incarnation: "fixture", OriginTurnID: "origin", Socket: filepath.Join(s.config.Workspace, "native.sock"), SocketEpoch: "epoch", RuntimeSchema: 1}
			service := ciServiceFixture(t, s, owner)
			source := ciTestClaim(strings.Repeat("1", 32)).Source
			req := monitoroperation.StartRequest{ID: strings.Repeat("1", 32), Brief: "Observe the exact fixture run", Source: source}
			if _, err := service.StartManaged(context.Background(), owner.RootThreadID, req); err != nil {
				t.Fatal(err)
			}
			if refusal == "arguments" {
				// Model a record admitted by the earlier generic source grammar.
				// Its ordinary persisted reader/replay remains supported.
				record, err := service.Store.GetObserver(context.Background(), req.ID)
				if err != nil {
					t.Fatal(err)
				}
				req.Source.WorkflowID = "Integration Tests"
				record.Source = req.Source
				record.SourceDigest = monitoroperation.SourceDigest(req.Source)
				requestBytes, _ := json.Marshal(req)
				record.RequestDigest = fmt.Sprintf("%x", sha256.Sum256(requestBytes))
				data, _ := json.Marshal(record)
				path := filepath.Join(s.config.UserStateRoot, "monitor-operations", fmt.Sprintf("%x", sha256.Sum256([]byte(s.config.Workspace))), "observers", req.ID+".json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := service.StartManaged(context.Background(), owner.RootThreadID, req); err != nil {
					t.Fatal("historical direct source no longer replays", err)
				}
			} else {
				checks := 0
				service.Admit = func(context.Context, monitoroperation.Owner) error {
					checks++
					if checks > 1 {
						return errors.New("provider admission refused before Start")
					}
					return nil
				}
			}
			called := filepath.Join(t.TempDir(), "called")
			s.config.GH = ciTestExecutable(t, "touch '"+called+"'")
			cursor := 0
			if err := s.runCIObservations(context.Background(), service, &cursor, time.Second, time.Second); err == nil {
				t.Fatal("worker hid the refusal")
			}
			record, err := service.Store.GetObserver(context.Background(), req.ID)
			if err != nil || record.Poll == nil || record.Poll.State != "joined" || record.Poll.OwnerEpoch != s.monitorCIEpoch || record.Poll.Handle == "" || record.Poll.FinishedAt == nil || record.Poll.ExitStatus != nil || record.Remote != nil || len(record.Events) != 0 {
				t.Fatalf("unsent proof not durably preserved: %+v %v", record, err)
			}
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused request started a process", err)
			}
			local, err := (monitoroperation.Lifecycle{Store: service.Store, Observer: s.monitorCI}).AssessLocalSafety(context.Background(), owner.Slug, owner.RootThreadID)
			if err != nil || local.State != "quiesceable" {
				t.Fatal("known unsent owner became a permanent unknown fence", local, err)
			}
		})
	}
}

func TestMonitorCIPreStartUnknownBindingRemainsUnresolved(t *testing.T) {
	for _, mismatch := range []string{"source", "epoch", "duplicate"} {
		t.Run(mismatch, func(t *testing.T) {
			claim := ciTestClaim(strings.Repeat("1", 32))
			source := claim.Source
			controller := &ciPollController{}
			switch mismatch {
			case "source":
				source.Commit = strings.Repeat("b", 40)
			case "epoch":
				claim.Poll.OwnerEpoch = ""
			case "duplicate":
				controller.calls = map[string]*ownedCIPoll{claim.ID: {receipt: *claim.Poll, done: make(chan struct{})}}
			}
			called := filepath.Join(t.TempDir(), "called")
			observer := ciPollObserver{controller: controller, claim: claim, executable: ciTestExecutable(t, "touch '"+called+"'"), publish: func(monitoroperation.PollReceipt) error { t.Fatal("unknown call published execution"); return nil }}
			_, receipt, err := observer.Poll(context.Background(), source)
			if err == nil || receipt.State == "joined" || receipt.FinishedAt != nil {
				t.Fatal("mismatched or duplicate owner acquired unsent proof", receipt, err)
			}
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unknown call started a process", err)
			}
		})
	}
}
