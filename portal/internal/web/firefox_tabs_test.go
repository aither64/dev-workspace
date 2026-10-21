package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// This is a disposable transport/visibility fixture, not a real model backend.
// The actual HTTP handlers, browser assets, durable browser retry identity and
// receipt reconciliation run unchanged. A private fsynced native stand-in journal
// proves that the lost response/retry dispatches only one accepted turn.
type firefoxTabsCodex struct {
	*browserContractCodex
	muTabs   sync.Mutex
	watchers map[chan struct{}]struct{}
	revision atomic.Uint64
	question atomic.Bool
	journal  string
}

func (c *firefoxTabsCodex) PendingRevision(string) uint64 { return c.revision.Load() }

func (c *firefoxTabsCodex) broadcast(invalidate bool) {
	if invalidate {
		c.revision.Add(1)
	}
	c.muTabs.Lock()
	defer c.muTabs.Unlock()
	for channel := range c.watchers {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

func (c *firefoxTabsCodex) Subscribe(context.Context, string) (<-chan struct{}, func(), error) {
	channel := make(chan struct{}, 1)
	c.muTabs.Lock()
	c.watchers[channel] = struct{}{}
	c.muTabs.Unlock()
	return channel, func() { c.muTabs.Lock(); delete(c.watchers, channel); c.muTabs.Unlock() }, nil
}

func (c *firefoxTabsCodex) PromptsWithItems(context.Context, string) ([]codex.Prompt, error) {
	if !c.question.Load() {
		return []codex.Prompt{}, nil
	}
	return []codex.Prompt{{ID: "question-1", Token: "firefox-offer", Method: "item/tool/requestUserInput", Kind: "userInput",
		ThreadID: "thread-1", TurnID: "turn-1", ItemID: "question-1", AuthorityAvailable: true, IsBlocking: true,
		Questions: []codex.Question{{ID: "choice", Header: "Firefox fixture", Question: "Disposable Firefox question", IsOther: true,
			Options: []codex.Option{{Label: "First", Description: "Disposable choice"}}}}}}, nil
}

func (c *firefoxTabsCodex) Send(ctx context.Context, thread, text, id, action string) (codex.SendReceipt, error) {
	receipt, err := c.browserContractCodex.Send(ctx, thread, text, id, action)
	if err != nil {
		return receipt, err
	}
	file, err := os.OpenFile(c.journal, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	if _, err := fmt.Fprintln(file, id); err != nil {
		return receipt, err
	}
	return receipt, file.Sync()
}

type firefoxLaneCount struct {
	Count, Active, Maximum, Failed int
}

func TestFirefoxFixtureJournalReconcilesOriginalAttempt(t *testing.T) {
	c := &firefoxTabsCodex{browserContractCodex: &browserContractCodex{}, journal: filepath.Join(t.TempDir(), "accepted-turns")}
	receipt, err := c.Send(context.Background(), "thread-1", "disposable input", "attempt-1", "")
	if err != nil {
		t.Fatal(err)
	}
	replay, found, err := c.ReconcileSend(context.Background(), "thread-1", "disposable input", "attempt-1", "")
	if err != nil || !found || replay != receipt || c.sendCount != 1 {
		t.Fatal("reconciliation redispatched fixture turn", err)
	}
	data, err := os.ReadFile(c.journal)
	if err != nil || strings.Count(string(data), "\n") != 1 {
		t.Fatal("fixture acceptance journal missing", err)
	}
}

func TestFirefoxFixtureReadiness(t *testing.T) {
	output, err := exec.Command("node", "--test", "firefox_tabs_readiness_test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("Firefox restoration readiness: %v\n%s", err, output)
	}
}

func TestFirefoxTabs(t *testing.T) {
	if os.Getenv("PORTAL_FIREFOX_TABS_TEST") != "1" {
		t.Skip("set PORTAL_FIREFOX_TABS_TEST=1 after review with ordinary Firefox, Geckodriver and a display")
	}
	runFirefoxFixture(t, false)
}

func TestFirefoxReadBudget(t *testing.T) {
	if os.Getenv("PORTAL_FIREFOX_READ_BUDGET_TEST") != "1" {
		t.Skip("set PORTAL_FIREFOX_READ_BUDGET_TEST=1 after review with ordinary Firefox, Geckodriver and a display")
	}
	runFirefoxFixture(t, true)
}

func runFirefoxFixture(t *testing.T, readBudget bool) {
	t.Helper()
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	controller := &firefoxTabsCodex{browserContractCodex: &browserContractCodex{emptyPrompts: true,
		transcript: codex.Transcript{ThreadID: "thread-1", Status: "active", CollaborationMode: "default", Model: "model-1", ReasoningEffort: "medium"}},
		watchers: make(map[chan struct{}]struct{}), journal: filepath.Join(t.TempDir(), "accepted-turns")}
	server.config.Codex = controller
	store, err := teamruntime.NewStore(server.config.UserStateRoot, server.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(context.Background(), "example", "thread-1", true, func(roster *teamruntime.Roster) error {
		roster.Members = append(roster.Members, teamruntime.Member{Address: "implementer0", Role: "implementer", Thread: "member-thread", Model: "model-1", Effort: "medium", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewUnstartedServer(nil)
	server.config.BaseURL = "https://" + httpServer.Listener.Addr().String()
	server.trustedOrigins = []string{server.config.BaseURL}
	server.conversation, err = conversation.NewHandler(conversation.Options{AllowedOrigins: server.trustedOrigins,
		BasePath: "/codex", Shutdown: server.stopping, Resolver: conversation.ResolverFunc(server.resolveConversation)})
	if err != nil {
		t.Fatal(err)
	}
	server.uploadHandler, err = conversation.NewUploadHandler(conversation.UploadOptions{AllowedOrigins: server.trustedOrigins,
		BasePath: "/uploads", Resolve: server.resolveUploads})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := make(map[string]map[string]*firefoxLaneCount)
	var failPending, lostResponse, lossInjected, lossHijackError atomic.Bool
	var sendAttempts, lastHandlerStatus atomic.Int64
	handler := server.Handler()
	httpServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/firefox/status" {
			mu.Lock()
			defer mu.Unlock()
			journal, _ := os.ReadFile(controller.journal)
			controller.mu.Lock()
			dispatches := controller.sendCount
			controller.mu.Unlock()
			server.writeJSON(w, 200, map[string]any{"fixture": "portal-firefox-tabs-v1", "counts": counts,
				"sendAttempts": sendAttempts.Load(), "dispatches": dispatches, "journalEntries": strings.Count(string(journal), "\n"),
				"lastHandlerStatus": lastHandlerStatus.Load(), "lossAttempted": lostResponse.Load(),
				"lossInjected": lossInjected.Load(), "hijackError": lossHijackError.Load()})
			return
		}
		if r.URL.Path == "/fixture/firefox/control" && r.Method == "POST" {
			switch r.URL.Query().Get("action") {
			case "question":
				controller.question.Store(true)
			case "clear-question":
				controller.question.Store(false)
			case "fail-pending":
				failPending.Store(true)
			default:
				http.Error(w, "unknown fixture action", 400)
				return
			}
			controller.broadcast(true)
			w.WriteHeader(204)
			return
		}
		lane := readDiagnosticLane(r)
		if strings.HasSuffix(r.URL.Path, "/events") {
			lane = "events"
		} else if strings.HasSuffix(r.URL.Path, "/attention") || strings.HasSuffix(r.URL.Path, "/coordination") {
			lane = filepath.Base(r.URL.Path)
		}
		referer, _ := url.Parse(r.Referer())
		document := ""
		if referer != nil {
			document = referer.Query().Get("fixtureTab")
		}
		var count *firefoxLaneCount
		if document != "" && lane != "" {
			mu.Lock()
			if counts[document] == nil {
				counts[document] = make(map[string]*firefoxLaneCount)
			}
			count = counts[document][lane]
			if count == nil {
				count = &firefoxLaneCount{}
				counts[document][lane] = count
			}
			count.Count++
			count.Active++
			if count.Active > count.Maximum {
				count.Maximum = count.Active
			}
			mu.Unlock()
			defer func() { mu.Lock(); count.Active--; mu.Unlock() }()
		}
		if lane == "pending" && failPending.CompareAndSwap(true, false) {
			if count != nil {
				mu.Lock()
				count.Failed++
				mu.Unlock()
			}
			server.writeJSON(w, 503, map[string]string{"error": "Disposable pending read failure"})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/message") {
			sendAttempts.Add(1)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			lastHandlerStatus.Store(int64(response.Code))
			if response.Code < 300 && lostResponse.CompareAndSwap(false, true) {
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					lossHijackError.Store(true)
					t.Error("fixture could not lose the accepted response")
					return
				}
				_ = connection.Close()
				lossInjected.Store(true)
				return
			}
			for key, values := range response.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		handler.ServeHTTP(w, r)
	})
	httpServer.StartTLS()
	defer httpServer.Close()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-ticker.C:
				controller.broadcast(false)
			case <-stop:
				return
			}
		}
	}()
	command := exec.Command("node", "firefox_tabs_acceptance.cjs", httpServer.URL)
	if readBudget {
		command.Args = append(command.Args, "--read-budget")
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ordinary Firefox acceptance: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
