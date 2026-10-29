package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/communication"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type communicationActivityClient struct {
	communicationTestClient
	metadata                     codex.ThreadMetadata
	activity                     codex.ActivitySnapshot
	activityErr                  error
	metadataReads, activityReads int
	sends, reconciles            int
	lost                         bool
}

func (c *communicationActivityClient) ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error) {
	c.metadataReads++
	return c.metadata, nil
}

func (c *communicationActivityClient) ReadActivity(context.Context, string) (codex.ActivitySnapshot, error) {
	c.activityReads++
	return c.activity, c.activityErr
}

func (c *communicationActivityClient) SendWithOptions(_ context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	c.sends++
	if c.lost {
		return codex.SendReceipt{}, errors.New("lost acknowledgement")
	}
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "delivered"}, nil
}

func (c *communicationActivityClient) ReconcileSendWithOptions(_ context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, bool, error) {
	c.reconciles++
	return codex.SendReceipt{ClientUserMessageID: id, TurnID: "delivered"}, true, nil
}

type communicationActivityObserver struct {
	testActivityObserver
	snapshot codex.ActivitySnapshot
	err      error
	reads    int
}

func (o *communicationActivityObserver) ReadActivity(context.Context, string) (codex.ActivitySnapshot, error) {
	o.reads++
	return o.snapshot, o.err
}

func communicationActivityFixture(t *testing.T) (*Server, *communicationActivityClient, *communicationActivityObserver, communication.Service, communication.Identity, string) {
	t.Helper()
	s := newTestServer(t)
	// Keep the disposable Unix socket below the platform's path length limit.
	root, err := os.MkdirTemp("", "communication-activity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	s.config.CodexSocket = filepath.Join(root, "s")
	listener, err := net.Listen("unix", s.config.CodexSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for _, slug := range []string{"sender", "receiver"} {
		dir := filepath.Join(s.config.Workspace, "work", slug)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		writeWebTrackingFiles(t, dir, "active")
		manifest := "schema: 1\nslug: " + slug + "\nrepositories: []\nartifacts: []\ncodex:\n  thread_id: " + slug + "-thread\n"
		if err := os.WriteFile(filepath.Join(dir, "portal.yml"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
	}
	authority := session.RuntimeAuthority{Schema: 1, State: "ready", Slug: "receiver", Workspace: s.config.Workspace,
		TmuxSocket: filepath.Join(root, "tmux"), TmuxSessionID: "$1", CodexThreadID: "receiver-thread",
		CodexSocketPath: s.config.CodexSocket, CodexClientVersion: "0.163.0"}
	raw, _ := json.Marshal(authority)
	if err := os.WriteFile(filepath.Join(s.config.AuthorityDir, "receiver.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.recoveryStore().Set(context.Background(), "receiver", "receiver-thread", s.config.CodexSocket, true, true); err != nil {
		t.Fatal(err)
	}
	c := &communicationActivityClient{metadata: codex.ThreadMetadata{ID: "receiver-thread", Cwd: filepath.Join(s.config.Workspace, "work", "receiver")},
		activity: codex.ActivitySnapshot{ThreadID: "receiver-thread", CurrentState: "working"}}
	s.config.Codex = c
	o := &communicationActivityObserver{snapshot: codex.ActivitySnapshot{ThreadID: "receiver-thread", CurrentState: "working"}}
	s.activity = &activityMonitor{observer: o}
	service, err := s.communicationService()
	if err != nil {
		t.Fatal(err)
	}
	caller, err := communication.IdentityFor(s.config.Workspace, "sender", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), caller, communication.Request{Action: "allow", Peer: "receiver", Approval: "User approved this pair in fixture message user-1"}); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := service.Run(context.Background(), caller, communication.Request{Action: "send", Peer: "receiver", MessageID: id, Message: "Deliberately shared fixture message"}); err != nil {
		t.Fatal(err)
	}
	return s, c, o, service, caller, id
}

func TestCommunicationDispatchUsesFreshObserverAndOperationalDelivery(t *testing.T) {
	s, client, observer, service, caller, id := communicationActivityFixture(t)
	client.activityErr = errors.New("operational client has no activity recorder")
	// A warm display cache must not decide whether communication can proceed.
	observer.snapshot.CurrentState = "idle"
	if _, err := s.activity.ReadActivity(context.Background(), "receiver-thread"); err != nil {
		t.Fatal(err)
	}
	observer.snapshot.CurrentState = "working"
	client.lost = true
	if err := service.Dispatch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	view, err := service.Read(context.Background(), caller)
	if err != nil || len(view.Messages) != 1 || view.Messages[0].Status != "submitting" {
		t.Fatalf("fresh working recipient was not submitted: %+v %v", view, err)
	}
	if client.activityReads != 0 || observer.reads != 2 || client.metadataReads != 1 || client.sends != 1 {
		t.Fatalf("incorrect native composition: operational=%+v observer reads=%d", client, observer.reads)
	}
	if err := service.Dispatch(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	view, err = service.Read(context.Background(), caller)
	if err != nil || view.Messages[0].Status != "delivered" || client.reconciles != 1 || client.sends != 1 {
		t.Fatalf("operational reconciliation was lost: %+v %v", view, err)
	}
}

func TestCommunicationDispatchIgnoresDisplayCacheAndBackoff(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot codex.ActivitySnapshot
		err      error
		allowed  bool
	}{
		{name: "working", snapshot: codex.ActivitySnapshot{CurrentState: "working"}, allowed: true},
		{name: "idle", snapshot: codex.ActivitySnapshot{CurrentState: "idle"}, allowed: true},
		{name: "automatic-wait", snapshot: codex.ActivitySnapshot{CurrentState: "waiting", WaitReason: "sleep"}, allowed: true},
		{name: "incomplete-history", snapshot: codex.ActivitySnapshot{CurrentState: "working", Refreshing: true, CoverageComplete: false}, allowed: true},
		{name: "user-input", snapshot: codex.ActivitySnapshot{CurrentState: "waiting", WaitReason: "userInput"}},
		{name: "approval", snapshot: codex.ActivitySnapshot{CurrentState: "waiting", WaitReason: "approval"}},
		{name: "unclassified", snapshot: codex.ActivitySnapshot{CurrentState: "unclassified"}},
		{name: "wrong-thread", snapshot: codex.ActivitySnapshot{ThreadID: "another-thread", CurrentState: "working"}},
		{name: "stale", snapshot: codex.ActivitySnapshot{CurrentState: "working", Stale: true}},
		{name: "error", err: errors.New("fresh activity failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, client, observer, service, caller, id := communicationActivityFixture(t)
			if _, err := s.activity.ReadActivity(context.Background(), "receiver-thread"); err != nil {
				t.Fatal(err)
			}
			// Exercise the display path's stale/backoff fallback as well as its
			// ordinary warm working cache. Neither is delivery evidence.
			s.activity.failures = map[string]activityReadFailure{"receiver-thread": {retryAt: time.Now().Add(time.Hour), err: errors.New("previous display failure")}}
			observer.snapshot, observer.err = test.snapshot, test.err
			if observer.snapshot.ThreadID == "" {
				observer.snapshot.ThreadID = "receiver-thread"
			}
			if err := service.Dispatch(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			view, err := service.Read(context.Background(), caller)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantSends := "held", 0
			if test.allowed {
				wantStatus, wantSends = "delivered", 1
			}
			if view.Messages[0].Status != wantStatus || client.sends != wantSends || client.activityReads != 0 || observer.reads != 2 {
				t.Fatalf("fresh admission mismatch: message=%+v sends=%d direct reads=%d observer reads=%d", view.Messages[0], client.sends, client.activityReads, observer.reads)
			}
		})
	}
}

func TestCommunicationDispatchKeepsPreActivityHolds(t *testing.T) {
	for _, hold := range []string{"provider", "runtime", "recovery", "socket", "metadata"} {
		t.Run(hold, func(t *testing.T) {
			s, client, observer, _, caller, id := communicationActivityFixture(t)
			switch hold {
			case "provider":
				stateRoot := s.config.UserStateRoot
				providerTestCatalog(t, s, "http://127.0.0.1:1")
				s.config.UserStateRoot = stateRoot
				path, err := codexproviders.StatePath(s.config.UserStateRoot, s.config.WorkspaceName)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				value := codexproviders.State{Schema: 1, Workspace: s.config.WorkspaceName, Root: s.config.Workspace, Socket: s.config.CodexSocket, Active: "pool",
					Operation: &codexproviders.Operation{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "pool", CatalogDigest: strings.Repeat("a", 64), Phase: "recovery-required"}}
				raw, _ := json.Marshal(value)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.requireProviderAdmission("receiver"); err == nil || !strings.Contains(err.Error(), "provider transition is reserved") {
					t.Fatalf("fixture did not establish a provider reservation: %v", err)
				}
			case "runtime":
				if err := os.Remove(filepath.Join(s.config.AuthorityDir, "receiver.json")); err != nil {
					t.Fatal(err)
				}
			case "recovery":
				if err := s.recoveryStore().Set(context.Background(), "receiver", "receiver-thread", s.config.CodexSocket, false, false); err != nil {
					t.Fatal(err)
				}
			case "socket":
				s.config.CodexSocket += "-wrong"
			case "metadata":
				client.metadata.Cwd += "-wrong"
			}
			service, err := s.communicationService()
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Dispatch(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			view, err := service.Read(context.Background(), caller)
			if err != nil || view.Messages[0].Status != "held" || client.sends != 0 || client.activityReads != 0 || observer.reads != 0 {
				t.Fatalf("activity bypassed existing hold: %+v %v", view, err)
			}
		})
	}
}

func TestCommunicationActivityReadGateCancellationHoldsDelivery(t *testing.T) {
	for _, gate := range []string{"global", "same-thread"} {
		t.Run(gate, func(t *testing.T) {
			s, client, observer, service, caller, id := communicationActivityFixture(t)
			count := activityReadConcurrency
			if gate == "same-thread" {
				count = 1
			}
			for i := 0; i < count; i++ {
				thread := string(rune('a' + i))
				if gate == "same-thread" {
					thread = "receiver-thread"
				}
				release, err := s.activity.acquireRead(context.Background(), thread)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			if err := service.Dispatch(ctx, id); err != nil {
				t.Fatal(err)
			}
			view, err := service.Read(context.Background(), caller)
			if err != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) || view.Messages[0].Status != "held" || client.sends != 0 || observer.reads != 0 {
				t.Fatalf("canceled admission reached observer or send: %+v %v", view, err)
			}
		})
	}
}

func TestCommunicationWithoutMonitorGuardsDirectActivity(t *testing.T) {
	for _, state := range []string{"working", "stale", "wrong-thread"} {
		t.Run(state, func(t *testing.T) {
			s, client, _, _, caller, id := communicationActivityFixture(t)
			s.activity = nil
			if state == "stale" {
				client.activity.Stale = true
			} else if state == "wrong-thread" {
				client.activity.ThreadID = "another-thread"
			}
			service, err := s.communicationService()
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Dispatch(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			view, err := service.Read(context.Background(), caller)
			wantStatus := "held"
			if state == "working" {
				wantStatus = "delivered"
			}
			if err != nil || view.Messages[0].Status != wantStatus || client.activityReads != 1 || (client.sends == 1) != (state == "working") {
				t.Fatalf("direct activity behavior mismatch: %+v %v", view, err)
			}
		})
	}
}
