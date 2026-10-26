package progressobserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProgressSchedulingRequiresActiveWork(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	state := State{LastCheck: now.Add(-Interval)}
	if state.Due(now, false) {
		t.Fatal("recent idle work woke observer")
	}
	if state.Due(now.Add(24*time.Hour), false) {
		t.Fatal("long-idle session woke observer")
	}
	if !state.Due(now, true) {
		t.Fatal("active long-running work was ignored")
	}
	if state.Due(now.Add(-time.Second), true) {
		t.Fatal("check ran before interval")
	}
	state.Paused = true
	if state.Due(now, true) {
		t.Fatal("paused check ran")
	}
	state.Paused = false
	state.Pending = &Pending{ID: "already-running"}
	if state.Due(now, true) {
		t.Fatal("overlapping check ran")
	}
}
func TestProgressConcernRequiresObservedResolution(t *testing.T) {
	var s State
	r := Result{Status: "concern", Key: "verification-loop", Summary: "Repeated equivalent checks.", Recommendation: "Run the missing acceptance check, then deliver."}
	if !s.Apply(r) || !s.Warning() {
		t.Fatal("first concern not visible")
	}
	s.Concern.Response = "I will simplify."
	if !s.Apply(r) || s.Concern.Count != 2 || s.Concern.Response == "" {
		t.Fatal("repeat did not challenge or lost response")
	}
	for _, result := range []Result{
		{Status: "concern", Key: "different", Summary: "Another issue", Recommendation: "Fix"},
		{Status: "on_track"},
		{Status: "on_track", Evidence: "Lead acknowledged the concern."},
		{Status: "on_track", Evidence: "Checked diff", Resolution: &Resolution{Key: "different", Evidence: "Done"}},
		{Status: "unavailable", Summary: "Diff unavailable"},
	} {
		s.Apply(result)
		if s.Concern == nil || !s.Warning() || s.Unavailable == "" {
			t.Fatal("unsupported resolution cleared concern", result)
		}
	}
	s.Apply(Result{Status: "on_track", Evidence: "Inspected final diff and completed targeted check.", Resolution: &Resolution{Key: r.Key, Evidence: "Custom inspector deleted; ordinary Nix query completed and package delivered."}})
	if s.Concern != nil || s.Warning() || s.Unavailable != "" {
		t.Fatal("observed resolution did not clear concern")
	}
}

func TestProgressSessionIntervals(t *testing.T) {
	now := time.Now()
	for _, minutes := range []int{1, 7, 60, 1440} {
		s := State{IntervalMinutes: minutes, LastCheck: now}
		at := now.Add(time.Duration(minutes) * time.Minute)
		if s.Due(at.Add(-time.Second), true) || !s.Due(at, true) || s.Due(at, false) {
			t.Fatal("interval or idle gate ignored", minutes)
		}
	}
	if (State{}).CheckInterval() != Interval {
		t.Fatal("old records lost default interval")
	}
}
func TestProgressResultValidation(t *testing.T) {
	for _, text := range []string{`{"status":"on_track"}`, `{"status":"unavailable","summary":"Missing diff"}`, `{"status":"on_track","evidence":"Diff inspected","resolution":{"key":"loop","evidence":"Removed subsystem"}}`, `{"status":"concern","key":"loop","summary":"Evidence repeated.","recommendation":"Run the acceptance check once."}`} {
		if _, err := ParseResult(text); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{`{"status":"on_track","resolution":{"key":"loop","evidence":""}}`, `{"status":"unavailable"}`, `null`, `{"status":"approve"}`, `{"status":"on_track","key":"extra"}`, `{"status":"on_track"} {}`, `{"status":"on_track","unknown":true}`, `{"status":"concern","key":"loop","summary":"","recommendation":"Stop"}`, `{"status":"concern","key":"loop","summary":"` + strings.Repeat("word ", 151) + `","recommendation":"Stop"}`} {
		if _, err := ParseResult(text); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}
func TestProgressStoreRetainsPendingIdentityAcrossRestart(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root, "workspace")
	s, err := store.Load("lead")
	if err != nil {
		t.Fatal(err)
	}
	s.Pending = &Pending{ID: ID("check"), Thread: "observer", Message: "bounded original packet", Model: "saved-model", Effort: "xhigh", StartedAt: time.Now()}
	s.IntervalMinutes = 7
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewStore(root, "workspace").Load("lead")
	if err != nil || restarted.Pending.ID != s.Pending.ID || restarted.Pending.Message != s.Pending.Message || restarted.IntervalMinutes != 7 {
		t.Fatal(restarted, err)
	}
	other, err := store.Load("another-lead")
	if err != nil || other.Pending != nil {
		t.Fatal("cross-session state", other, err)
	}
	files, _ := filepath.Glob(filepath.Join(store.Directory, "*.json"))
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private state permissions", info, err)
	}
}
