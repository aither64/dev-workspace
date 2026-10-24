package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

func retryRequest(s *Server, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.config.BaseURL)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func retryPolicyPath(f *planDeliveryFixture) string {
	sum := sha256.Sum256([]byte(f.s.config.Workspace))
	return filepath.Join(f.s.config.UserStateRoot, "turn-retry-policy", hex.EncodeToString(sum[:]), "example.json")
}

func addPolicyMember(t *testing.T, f *planDeliveryFixture) {
	t.Helper()
	service, err := f.s.teamService("example")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Store.Update(context.Background(), "example", "thread-1", true, func(r *teamruntime.Roster) error {
		r.Members = []teamruntime.Member{{Address: "implementer0", Role: "implementer", Thread: "member-thread", Model: "gpt-6.1-sol", Effort: "xhigh", Behavior: "implementer", Access: "workspace_write", State: "ready", AddedAt: time.Now().UTC()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.client.RetryPolicyResolver = func(ctx context.Context, thread string) (codex.ThreadPolicy, error) {
		if thread == "member-thread" {
			roster, err := service.Store.Load("example", "thread-1")
			if err != nil {
				return codex.ThreadPolicy{}, err
			}
			return service.MemberTurnPolicy("example", "thread-1", roster.Members[0])
		}
		return codex.ThreadPolicy{}, nil
	}
	if err := (session.RecoveryStore{Root: f.s.config.UserStateRoot, Workspace: f.s.config.Workspace}).Update(context.Background(), "example", func(r *session.Recovery) error {
		r.ActiveThreads = append(r.ActiveThreads, "member-thread")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetryPolicyActualHTTPBootstrapReadOnlyInheritanceAndNoop(t *testing.T) {
	f := newPlanDeliveryFixture(t)
	addPolicyMember(t, f)
	ctx := context.Background()
	memberScope, err := f.client.RetryScope(ctx, "member-thread")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.client.RetryStore().Lock(ctx, memberScope)
	if err != nil {
		t.Fatal(err)
	}
	tx.State.Enabled = false
	beforeLedger, _ := json.Marshal(tx.State)
	if err := tx.Save(); err != nil {
		t.Fatal(err)
	}
	tx.Close()
	if w := retryRequest(f.s, ctx, "GET", "/api/sessions/example/retry-policy", ""); w.Code != 503 {
		t.Fatal("GET bootstrapped policy", w.Code, w.Body.String())
	}
	p, err := f.s.transactRetryPolicy(ctx, f.client, "example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Capacity || p.Cybersecurity || len(p.Threads) != 2 {
		t.Fatal("member opt-out not inherited", p)
	}
	afterLedger, _ := f.client.RetryStore().Load(memberScope)
	afterData, _ := json.Marshal(afterLedger)
	if !bytes.Equal(beforeLedger, afterData) {
		t.Fatal("bootstrap rewrote legacy ledger")
	}
	rootScope, _ := f.client.RetryScope(ctx, "thread-1")
	if _, err := f.client.RetryStore().Load(rootScope); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bootstrap created root ledger", err)
	}
	runtime, err := session.LockRuntimeExclusive(f.s.config.AuthorityDir, "example")
	if err != nil {
		t.Fatal(err)
	}
	// A real held EX cannot block a GET: polling never enters runtime admission.
	w := retryRequest(f.s, ctx, "GET", "/api/sessions/example/retry-policy", "")
	runtime.Close()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	before, _ := os.Stat(retryPolicyPath(f))
	data, _ := os.ReadFile(retryPolicyPath(f))
	w = retryRequest(f.s, ctx, "POST", "/api/sessions/example/retry-policy", `{"capacity":false,"cybersecurity":false,"expectedRevision":"stale"}`)
	if w.Code != 200 {
		t.Fatal("same-value stale retry failed", w.Code, w.Body.String())
	}
	after, _ := os.Stat(retryPolicyPath(f))
	if !os.SameFile(before, after) {
		t.Fatal("same-valued transaction rewrote sidecar")
	}
	w = retryRequest(f.s, ctx, "POST", "/api/sessions/example/retry-policy", `{"capacity":true,"cybersecurity":false,"expectedRevision":"stale"}`)
	if w.Code != 409 {
		t.Fatal("stale differing transaction accepted", w.Code)
	}
	if got, _ := os.ReadFile(retryPolicyPath(f)); !bytes.Equal(data, got) {
		t.Fatal("stale candidate mutated policy")
	}
	body, _ := json.Marshal(map[string]any{"capacity": true, "cybersecurity": false, "expectedRevision": p.Revision})
	w = retryRequest(f.s, ctx, "POST", "/api/sessions/example/retry-policy", string(body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	summary, _ := session.Find(f.s.config.Workspace, "example")
	identity, _ := policyScope(summary)
	p, err = policyStore(f.client).Load(identity)
	if err != nil || !p.Capacity || p.Cybersecurity || p.Revision == "" {
		t.Fatal(p, err)
	}
}

type policyProbeBackend struct {
	turnretry.Backend
	observe func(context.Context, turnretry.Scope) (turnretry.Observation, error)
}

func (b policyProbeBackend) Observe(ctx context.Context, scope turnretry.Scope) (turnretry.Observation, error) {
	return b.observe(ctx, scope)
}

type policyProbeClient struct {
	*planDeliveryClient
	backend turnretry.Backend
}

func (c *policyProbeClient) RetryBackend() turnretry.Backend { return c.backend }

type policyWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *policyWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestRetryPolicyEXWaitCancellationFreshGenerationAndTwoObservations(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline", "released", "generation", "authority", "lifecycle", "turnover", "active", "terminal"} {
		t.Run(mode, func(t *testing.T) {
			f := newPlanDeliveryFixture(t)
			ctx := context.Background()
			rootScope, err := f.client.RetryScope(ctx, "thread-1")
			if err != nil {
				t.Fatal(err)
			}
			legacy, err := f.client.RetryStore().Lock(ctx, rootScope)
			if err != nil {
				t.Fatal(err)
			}
			legacy.State.Enabled = false
			if err := legacy.Save(); err != nil {
				t.Fatal(err)
			}
			legacy.Close()

			p, err := f.s.transactRetryPolicy(ctx, f.client, "example", nil)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(retryPolicyPath(f))
			body, _ := json.Marshal(map[string]any{"capacity": true, "cybersecurity": true, "expectedRevision": p.Revision})
			if mode == "active" || mode == "terminal" || mode == "turnover" {
				f.wire.mu.Lock()
				f.wire.status = "inProgress"
				if mode == "terminal" {
					f.wire.status = "completed"
				}
				f.wire.mu.Unlock()
				calls := 0
				base := f.client.RetryBackend()
				probe := &policyProbeClient{planDeliveryClient: f.client}
				probe.backend = policyProbeBackend{Backend: base, observe: func(ctx context.Context, scope turnretry.Scope) (turnretry.Observation, error) {
					calls++
					o, err := base.Observe(ctx, scope)
					if mode == "turnover" && calls == 1 {
						f.wire.mu.Lock()
						f.wire.latest = "replacement"
						f.wire.mu.Unlock()
					}
					return o, err
				}}
				f.s.config.Codex = probe
				w := retryRequest(f.s, ctx, "POST", "/api/sessions/example/retry-policy", string(body))
				if mode == "turnover" {
					if w.Code != 503 {
						t.Fatal("turnover committed", w.Code, w.Body.String())
					}
				} else {
					if w.Code != 200 || calls != 2 {
						t.Fatal("two observation boundary missing", w.Code, calls, w.Body.String())
					}
					summary, _ := session.Find(f.s.config.Workspace, "example")
					scope, _ := policyScope(summary)
					saved, _ := policyStore(f.client).Load(scope)
					if saved.Thread("thread-1").Capacity.Active != (mode == "active") {
						t.Fatal("wrong active boundary", saved)
					}
					return
				}
			} else {
				lock, err := session.LockRuntimeShared(f.s.config.AuthorityDir, "example")
				if err != nil {
					t.Fatal(err)
				}
				budget := 3 * time.Second
				if mode == "deadline" {
					budget = 40 * time.Millisecond
				}
				bounded, cancel := context.WithTimeout(ctx, budget)
				defer cancel()
				waiting := &policyWaitContext{Context: bounded, waiting: make(chan struct{})}
				result := make(chan error, 1)
				yes := true
				go func() {
					_, err := f.s.transactRetryPolicy(waiting, f.client, "example", &retryPolicyRequest{Capacity: &yes, Cybersecurity: &yes, ExpectedRevision: p.Revision})
					result <- err
				}()
				select {
				case <-waiting.waiting:
				case err := <-result:
					lock.Close()
					t.Fatal("transaction returned before EX contention", err)
				case <-bounded.Done():
					lock.Close()
					t.Fatal("no EX contention barrier")
				}
				switch mode {
				case "cancel":
					cancel()
				case "deadline":
					err := <-result
					lock.Close()
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("deadline granted EX", err)
					}
					if got, _ := os.ReadFile(retryPolicyPath(f)); !bytes.Equal(before, got) {
						t.Fatal("deadline changed policy")
					}
					return
				case "generation":
					if err := os.Remove(f.s.config.HostProfile); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), f.s.config.HostProfile); err != nil {
						t.Fatal(err)
					}
				case "authority":
					if err := os.Remove(filepath.Join(f.s.config.AuthorityDir, "example.json")); err != nil {
						t.Fatal(err)
					}
				case "lifecycle":
					writeWebTrackingFiles(t, f.wire.cwd, "complete")
				}
				lock.Close()
				err = <-result
				if mode == "released" {
					if err != nil {
						t.Fatal("released transaction failed", err)
					}
					return
				}
				if err == nil {
					t.Fatal("post-wait authority accepted", mode)
				}

			}
			if got, _ := os.ReadFile(retryPolicyPath(f)); !bytes.Equal(before, got) {
				t.Fatal("refused policy partially saved")
			}
			f.wire.mu.Lock()
			defer f.wire.mu.Unlock()
			if f.wire.starts != 0 || f.wire.updates != 0 {
				t.Fatal("policy dispatched or changed settings")
			}
		})
	}
}

func TestRetryPolicySidecarNeverGatesActualManualDelivery(t *testing.T) {
	for _, damage := range []string{"missing", "malformed", "full", "unwritable"} {
		for _, operation := range []string{"message", "queue", "member-message", "member-queue", "plan"} {
			t.Run(damage+"/"+operation, func(t *testing.T) {
				f := newPlanDeliveryFixture(t)
				addPolicyMember(t, f)
				ctx := context.Background()
				path := retryPolicyPath(f)
				if damage != "missing" {
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					data := []byte("invalid policy")
					if damage == "full" {
						data = bytes.Repeat([]byte("x"), turnretry.MaxPolicyBytes+1)
					}
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					if damage == "unwritable" {
						if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
							t.Fatal(err)
						}
						defer os.Chmod(filepath.Dir(path), 0700)
					}
				}
				before, _ := os.ReadFile(path)
				if operation == "plan" {
					w := f.request(ctx, planDeliveryID, "plan-turn", "same")
					if w.Code != 202 {
						t.Fatal("optional metadata blocked plan", w.Code, w.Body.String())
					}
					f.restart(t)
					w = f.request(ctx, planDeliveryID, "plan-turn", "recover")
					if w.Code != 202 {
						t.Fatal("optional metadata blocked exact plan recovery", w.Code, w.Body.String())
					}
				} else {
					id, route := "example", operation
					if strings.HasPrefix(operation, "member-") {
						id = "example~implementer0"
						route = strings.TrimPrefix(operation, "member-")
					}
					w := retryRequest(f.s, ctx, "POST", "/codex/conversations/"+id+"/"+route, `{"message":"manual message","clientUserMessageId":"00000000-0000-4000-8000-000000000093"}`)
					if w.Code != 202 {
						t.Fatal("optional metadata blocked manual delivery", w.Code, w.Body.String())
					}
				}
				after, err := os.ReadFile(path)
				if damage == "missing" && !errors.Is(err, os.ErrNotExist) || !bytes.Equal(before, after) {
					t.Fatal("manual delivery consulted or mutated sidecar", err)
				}
			})
		}
	}
}

// Compile-time assertions keep the actual optional A contract bound to B.
var _ conversation.RetryActivityProvider = retryProvider{}
