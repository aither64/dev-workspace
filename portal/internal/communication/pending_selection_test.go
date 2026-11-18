package communication

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPendingSelectionRefreshAndReconciliation(t *testing.T) {
	ctx := context.Background()
	s, ids, transport := fixture(t)
	allow(t, s, ids["a"], "b")
	id := send(t, s, ids["a"], "b", 1)
	check := func(reads uint64, count int) {
		t.Helper()
		ms, err := s.Pending(ctx)
		if err != nil || len(ms) != count || s.Store.pendingReads != reads {
			t.Fatalf("pending %d reads%d: %v", len(ms), s.Store.pendingReads, err)
		}
	}
	check(1, 1)
	check(1, 1)
	// External readiness changes without a file change; unchanged pending is retried.
	transport.held = "fixture hold"
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	check(2, 1)
	transport.held = ""
	transport.lost = true
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	check(3, 1)
	// An uncertain submission remains actionable and reconciles, never resends.
	transport.lost = false
	if err := s.Dispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	check(4, 0)
	check(4, 0)
	if transport.sends != 1 {
		t.Fatal("uncertain delivery resent")
	}
	path := filepath.Join(s.Store.directory, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	check(5, 0)
	tmp := filepath.Join(s.Store.directory, ".replacement")
	if err = os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	check(6, 0)
	restarted := s
	restarted.Store = &Store{directory: s.Store.directory, workspace: s.Store.workspace}
	if _, err = restarted.Pending(ctx); err != nil || restarted.Store.pendingReads != 1 {
		t.Fatal("restart did not discover", err)
	}
	if err = os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pending(ctx); err == nil {
		t.Fatal("malformed changed ledger served cached pending")
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	check(8, 0)
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(9, 0)
}
