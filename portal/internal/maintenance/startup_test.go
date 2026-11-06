package maintenance

import (
	"context"
	"testing"
)

func TestStartupGrantIsBoundedAndConsumedBeforeExec(t *testing.T) {
	c, _ := fixture(t)
	ctx := context.Background()
	state, e := c.Store.Startup()
	if e != nil || state.Enabled || state.Pending {
		t.Fatalf("%+v %v", state, e)
	}
	g := StartupGrant{Unit: "workspace-codex@test.service", Workspace: "/workspace", Native: "/native", ArgvDigest: "digest"}
	if e := c.Store.StageStartup(ctx, testID, "/candidate", "source", true); e != nil {
		t.Fatal(e)
	}
	state, e = c.Store.Startup()
	if e != nil || state.Enabled || !state.Pending || !state.TargetEnabled {
		t.Fatalf("%+v %v", state, e)
	}
	if _, e = c.Store.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "invocation", g, startupTestAttempt()); e == nil {
		t.Fatal("pending ordinary launch had no grant")
	}
	if e := c.Store.AuthorizeStartupGrants(ctx, testID, "/candidate", "selected", []StartupGrant{g}); e != nil {
		t.Fatal(e)
	}
	wrong := g
	wrong.ArgvDigest = "other"
	if _, e := c.Store.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "invocation", wrong, startupTestAttempt()); e == nil {
		t.Fatal("changed argv accepted")
	}
	if enabled, e := c.Store.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "invocation", g, startupTestAttempt()); e != nil || !enabled {
		t.Fatalf("%t %v", enabled, e)
	}
	if _, e := c.Store.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "replacement", g, startupTestAttempt()); e == nil {
		t.Fatal("replacement replayed consumed grant")
	}
	if e := c.Store.AuthorizeStartupGrants(ctx, testID, "/candidate", "selected", []StartupGrant{g}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Store.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "replacement", g, startupTestAttempt()); e == nil {
		t.Fatal("authorize retry erased consumption")
	}
}

func TestStartupTargetCannotChangeDuringRetry(t *testing.T) {
	c, _ := fixture(t)
	ctx := context.Background()
	if e := c.Store.StageStartup(ctx, testID, "/same-package", "old", true); e != nil {
		t.Fatal(e)
	}
	if e := c.Store.StageStartup(ctx, testID, "/same-package", "old", false); e == nil {
		t.Fatal("changed startup target")
	}
}

func TestCompletedStartupRetryDoesNotReissueConsumedGrant(t *testing.T) {
	c, _ := fixture(t)
	ctx := context.Background()
	g := StartupGrant{Unit: "unit", Workspace: "/workspace", Native: "/native", ArgvDigest: "digest"}
	if e := c.Store.StageStartup(ctx, testID, "/candidate", "source", true); e != nil {
		t.Fatal(e)
	}
	if e := c.Store.AuthorizeStartupGrants(ctx, testID, "/candidate", "selected", []StartupGrant{g}); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Store.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "invocation", g, startupTestAttempt()); e != nil {
		t.Fatal(e)
	}
	readyStartupTest(t, c.Store)
	if e := c.Store.FinishStartup(ctx, testID, "/candidate", "selected"); e != nil {
		t.Fatal(e)
	}
	if e := c.Store.StageStartup(ctx, testID, "/candidate", "source", true); e != nil {
		t.Fatal(e)
	}
	state, e := c.Store.Startup()
	if e != nil || state.Pending || state.LaunchGrants[0].Invocation != "invocation" {
		t.Fatalf("%+v %v", state, e)
	}
}

func startupTestAttempt() StartupAttempt {
	return StartupAttempt{PID: 123, ProcessIdentity: "boot:123:start", Boot: "boot", Cgroup: "/user.slice/unit"}
}
func readyStartupTest(t *testing.T, s Store) {
	t.Helper()
	state, e := s.Startup()
	if e != nil {
		t.Fatal(e)
	}
	g := state.LaunchGrants[0]
	a := g.Attempts[len(g.Attempts)-1]
	if e = s.ReconcileStartupAttempt(context.Background(), state.OperationID, state.CandidatePackage, state.AuthorizedProfileToken, g, a, "ready", "/native-executable", "destination-epoch"); e != nil {
		t.Fatal(e)
	}
}

func TestTerminatedStartupReplacementRetainsEvidenceAndIsOneUse(t *testing.T) {
	c, _ := fixture(t)
	s := c.Store
	ctx := context.Background()
	g := StartupGrant{Unit: "unit", Workspace: "/workspace", Native: "/launcher", ArgvDigest: "argv"}
	if e := s.StageStartup(ctx, testID, "/candidate", "source", true); e != nil {
		t.Fatal(e)
	}
	if e := s.AuthorizeStartupGrants(ctx, testID, "/candidate", "selected", []StartupGrant{g}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "old", g, startupTestAttempt()); e != nil {
		t.Fatal(e)
	}
	state, _ := s.Startup()
	old := state.LaunchGrants[0]
	attempt := old.Attempts[0]
	foreign := old
	foreign.ArgvDigest = "changed"
	if e := s.ReconcileStartupAttempt(ctx, testID, "/candidate", "selected", foreign, attempt, "failed", "", ""); e == nil {
		t.Fatal("foreign tuple reauthorized")
	}
	if e := s.ReconcileStartupAttempt(ctx, testID, "/candidate", "selected", old, attempt, "failed", "", ""); e != nil {
		t.Fatal(e)
	}
	if e := s.ReconcileStartupAttempt(ctx, testID, "/candidate", "selected", old, attempt, "failed", "", ""); e != nil {
		t.Fatal("exact authorization retry", e)
	}
	state, _ = s.Startup()
	next := state.LaunchGrants[0]
	if next.Sequence != 2 || next.Invocation != "" || len(next.Attempts) != 1 || next.Attempts[0].Invocation != "old" || next.Attempts[0].Disposition != "failed" {
		t.Fatal(next)
	}
	if e := s.AuthorizeStartupGrants(ctx, testID, "/candidate", "selected", []StartupGrant{g}); e != nil {
		t.Fatal(e)
	}
	proof := startupTestAttempt()
	proof.PID = 124
	proof.ProcessIdentity = "boot:124:start"
	if _, e := s.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "replacement", g, proof); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "third", g, proof); e == nil {
		t.Fatal("replacement consumption replayed")
	}
	state, _ = s.Startup()
	if len(state.LaunchGrants[0].Attempts) != 2 || state.LaunchGrants[0].Attempts[1].Sequence != 2 {
		t.Fatal(state)
	}
	readyStartupTest(t, s)
	state, _ = s.Startup()
	ready := state.LaunchGrants[0]
	if e := s.ReconcileStartupAttempt(ctx, testID, "/candidate", "selected", ready, ready.Attempts[1], "failed", "", ""); e == nil {
		t.Fatal("successful startup blindly replaced")
	}
}

func TestStartupReplacementLeavesSuccessfulOtherServiceUntouched(t *testing.T) {
	c, _ := fixture(t)
	s := c.Store
	ctx := context.Background()
	first := StartupGrant{Unit: "first", Workspace: "/one", Native: "/native", ArgvDigest: "one"}
	second := StartupGrant{Unit: "second", Workspace: "/two", Native: "/native", ArgvDigest: "two"}
	if e := s.StageStartup(ctx, testID, "/candidate", "source", true); e != nil {
		t.Fatal(e)
	}
	if e := s.AuthorizeStartupGrants(ctx, testID, "/candidate", "selected", []StartupGrant{first, second}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "first-invocation", first, startupTestAttempt()); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ConsumeStartupGrant(ctx, testID, "/candidate", "selected", "second-invocation", second, startupTestAttempt()); e != nil {
		t.Fatal(e)
	}
	state, _ := s.Startup()
	a, b := state.LaunchGrants[0], state.LaunchGrants[1]
	if e := s.ReconcileStartupAttempt(ctx, testID, "/candidate", "selected", a, a.Attempts[0], "ready", "/native", "first-epoch"); e != nil {
		t.Fatal(e)
	}
	if e := s.ReconcileStartupAttempt(ctx, testID, "/candidate", "selected", b, b.Attempts[0], "failed", "", ""); e != nil {
		t.Fatal(e)
	}
	state, _ = s.Startup()
	if state.LaunchGrants[0].Invocation != "first-invocation" || state.LaunchGrants[0].Sequence != 1 || state.LaunchGrants[0].Attempts[0].Disposition != "ready" || state.LaunchGrants[1].Sequence != 2 || state.LaunchGrants[1].Invocation != "" {
		t.Fatal(state)
	}
}
