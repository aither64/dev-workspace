package turnretry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func policyFixture(t *testing.T) (PolicyStore, PolicyScope) {
	t.Helper()
	return PolicyStore{Root: t.TempDir(), Workspace: "/workspace"}, PolicyScope{Workspace: "/workspace", Slug: "2026-10-22-test", Root: "root", Incarnation: "incarnation"}
}

func createPolicy(t *testing.T, store PolicyStore, scope PolicyScope) Policy {
	t.Helper()
	p, err := store.Update(context.Background(), scope, true, func(p *Policy) error {
		p.Threads = []ThreadPolicy{{Thread: "root", Suppressed: []Suppression{}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicyStoreStrictIdentityAndBounds(t *testing.T) {
	store, scope := policyFixture(t)
	p := createPolicy(t, store, scope)
	original, _ := os.ReadFile(store.path(scope))
	if store.path(scope) != filepath.Join(store.Root, "turn-retry-policy", hash(scope.Workspace), scope.Slug+".json") {
		t.Fatal("path contract changed")
	}
	for _, change := range []func(*PolicyScope){func(s *PolicyScope) { s.Incarnation = "replacement" }, func(s *PolicyScope) { s.Root = "other" }} {
		other := scope
		change(&other)
		if _, err := store.Load(other); err == nil {
			t.Fatal("identity adopted")
		}
		if _, err := store.Update(context.Background(), other, true, func(*Policy) error { return nil }); err == nil {
			t.Fatal("mismatch overwritten")
		}
	}
	for name, data := range map[string][]byte{
		"unknown":        bytes.Replace(original, []byte(`"schema":1`), []byte(`"schema":1,"extra":true`), 1),
		"duplicate":      bytes.Replace(original, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1),
		"missing-choice": bytes.Replace(original, []byte(`"capacity":true,`), nil, 1),
		"null-choice":    bytes.Replace(original, []byte(`"capacity":true`), []byte(`"capacity":null`), 1),
		"trailing":       append(append([]byte{}, original...), []byte(`{}`)...),
		"oversize":       bytes.Repeat([]byte(" "), MaxPolicyBytes+1),
		"empty":          {},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(store.path(scope), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(scope); err == nil {
				t.Fatal("invalid policy accepted")
			}
			if _, err := store.Update(context.Background(), scope, true, func(*Policy) error { return nil }); err == nil {
				t.Fatal("invalid policy replaced")
			}
			retained, _ := os.ReadFile(store.path(scope))
			if !bytes.Equal(data, retained) {
				t.Fatal("evidence changed")
			}
		})
	}
	if err := os.WriteFile(store.path(scope), original, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Policy)
	}{
		{"threads", func(p *Policy) {
			for len(p.Threads) <= MaxPolicyThreads {
				p.Threads = append(p.Threads, ThreadPolicy{Thread: strings.Repeat("x", len(p.Threads)+1), Suppressed: []Suppression{}})
			}
		}},
		{"bytes", func(p *Policy) {
			p.Threads = nil
			for i := 0; i < MaxPolicyThreads; i++ {
				p.Threads = append(p.Threads, ThreadPolicy{Thread: strings.Repeat("x", i+1), Capacity: Enrollment{Turn: strings.Repeat("y", 4096)}, Cybersecurity: Enrollment{Turn: strings.Repeat("z", 4096)}, Suppressed: []Suppression{}})
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Update(context.Background(), scope, false, func(p *Policy) error { test.mutate(p); return nil })
			if err == nil {
				t.Fatal("bound ignored")
			}
			got, _ := store.Load(scope)
			if got.Revision != p.Revision || len(got.Threads) != 1 {
				t.Fatal("partial candidate saved")
			}
		})
	}
	if err := os.Chmod(store.path(scope), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(scope); err == nil {
		t.Fatal("public state accepted")
	}
}

func TestPolicyStoreConcurrentCheckpointAndFiniteLock(t *testing.T) {
	store, scope := policyFixture(t)
	initial := createPolicy(t, store, scope)
	var wg sync.WaitGroup
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Update(context.Background(), scope, false, func(p *Policy) error {
				e := p.Thread("root")
				if e.Summary == nil {
					e.Summary = &Summary{Chain: "chain", Class: "capacity", Phase: "running", CountQuality: "exact", ConfirmedStarts: integer(0), TimingQuality: "unavailable"}
				}
				e.Summary.ConfirmedStarts = integer(*e.Summary.ConfirmedStarts + 1)
				return nil
			})
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := store.Load(scope)
	if err != nil || *p.Thread("root").Summary.ConfirmedStarts != 20 || p.Revision != initial.Revision {
		t.Fatalf("checkpoint lost: %+v %v", p, err)
	}
	lock, err := os.OpenFile(store.path(scope)+".lock", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.path(scope))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = store.Update(ctx, scope, false, func(p *Policy) error { p.Capacity = false; return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock did not cancel: %v", err)
	}
	after, _ := os.ReadFile(store.path(scope))
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled writer committed")
	}
	// Polling is lock-free and reads the prior complete atomic record.
	if _, err := store.Load(scope); err != nil {
		t.Fatal(err)
	}
}

func TestPolicySameValueNoWriteAndRejectedCandidate(t *testing.T) {
	store, scope := policyFixture(t)
	p := createPolicy(t, store, scope)
	before, _ := os.Stat(store.path(scope))
	data, _ := os.ReadFile(store.path(scope))
	_, err := store.Update(context.Background(), scope, false, func(p *Policy) error {
		return PreparePolicy(p, []PolicyThread{{State: New(fixtureScope())}}, true, true, "stale", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(store.path(scope))
	if !os.SameFile(before, after) {
		t.Fatal("same value rewrote policy")
	}
	_, err = store.Update(context.Background(), scope, false, func(p *Policy) error { p.Capacity = false; return errors.New("inventory failed") })
	if err == nil {
		t.Fatal("partial inventory accepted")
	}
	got, _ := os.ReadFile(store.path(scope))
	if !bytes.Equal(data, got) {
		t.Fatal("failure mutated policy")
	}
	var decoded Policy
	if json.Unmarshal(got, &decoded) != nil || decoded.Revision != p.Revision {
		t.Fatal("policy identity lost")
	}
}
