package main

import (
	"context"
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMonitorArchiveInheritedLockRequiresExactExclusiveOwner(t *testing.T) {
	for _, kind := range []string{"exact", "missing", "unheld", "shared", "wrong-inode", "foreign-description", "mode", "symlink", "wrong-path", "wrong-operation"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			authority := filepath.Join(root, "authority")
			if err := os.Mkdir(authority, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(authority, "test-session.lock")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if kind != "unheld" {
				mode := unix.LOCK_EX
				if kind == "shared" {
					mode = unix.LOCK_SH
				}
				if err := unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "archive")
			t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", path)
			t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", strconv.Itoa(int(f.Fd())))
			switch kind {
			case "missing":
				t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", "")
			case "wrong-operation":
				t.Setenv("DEV_SESSION_LIFECYCLE_OPERATION", "delete")
			case "wrong-path":
				t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_PATH", filepath.Join(root, "other.lock"))
			case "mode":
				if err := f.Chmod(0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "wrong-inode":
				other, err := os.OpenFile(filepath.Join(root, "other.lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", strconv.Itoa(int(other.Fd())))
			case "foreign-description":
				other, err := os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				t.Setenv("DEV_SESSION_LIFECYCLE_LOCK_FD", strconv.Itoa(int(other.Fd())))
			}
			inherited, err := inheritedMonitorArchiveLock(root, authority, "test-session")
			if kind != "exact" {
				if err == nil {
					_ = inherited.Close()
					t.Fatal("unproved descriptor accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := inherited.Close(); err != nil {
				t.Fatal(err)
			}
			probe, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			if err := unix.Flock(int(probe.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != unix.EWOULDBLOCK && err != unix.EAGAIN {
				t.Fatal("child released or downgraded parent's exclusion", err)
			}
		})
	}
}

func monitorObserverCLI(t *testing.T) (*monitoroperation.Service, monitoroperation.StartRequest, monitoroperation.Owner, string) {
	t.Helper()
	binding := monitorLeadFixture(t)
	store, err := monitoroperation.NewStore(binding.stateRoot, binding.workspace)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := monitoroperation.IdentityFor(binding.workspace, "test-session")
	if err != nil {
		t.Fatal(err)
	}
	owner.OriginTurnID, owner.Socket, owner.SocketEpoch, owner.RuntimeSchema = "origin", filepath.Join(binding.workspace, "native.sock"), "epoch", 1
	s := &monitoroperation.Service{Store: store, ObserverEpoch: "worker", ResolveLead: func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil }, Resolve: func(string) (monitoroperation.Owner, error) { return owner, nil }, LockSession: func(context.Context, string) (func(), error) { return func() {}, nil }, Admit: func(context.Context, monitoroperation.Owner) error { return nil }}
	req := monitoroperation.StartRequest{ID: strings.Repeat("1", 32), Brief: "Exact CI", Source: monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "example/project", WorkflowID: "12", RunID: "34", RunAttempt: 1, Commit: strings.Repeat("a", 40)}}
	if _, err := s.StartCIObservation(context.Background(), owner.RootThreadID, req); err != nil {
		t.Fatal(err)
	}
	return s, req, owner, binding.stateRoot
}
func TestMonitorArchiveSelectionPreservesPartialBeginLostReplyAndDetachedBinding(t *testing.T) {
	ctx := context.Background()
	s, req, owner, _ := monitorObserverCLI(t)
	r, _ := s.Store.GetObserver(ctx, req.ID)
	a := monitoroperation.ArchiveAuthority{Slug: owner.Slug, RootThreadID: owner.RootThreadID, TargetID: "v2-target", Mode: "explicit"}
	chosen, err := selectMonitorArchiveAuthority(ctx, s.Store, r.Workspace, a)
	if err != nil || len(chosen.OperationID) != 64 || strings.Trim(chosen.OperationID, "0123456789abcdef") != "" {
		t.Fatal(chosen, err)
	}
	l := monitoroperation.Lifecycle{Store: s.Store, AuthorizeArchive: func(context.Context, monitoroperation.ArchiveAuthority) error { return nil }}
	if err := l.BeginArchiveQuiescence(ctx, chosen); err != nil {
		t.Fatal(err)
	}
	// Another affected record has no binding yet, representing a partial Begin.
	second := req
	second.ID = strings.Repeat("2", 32)
	if _, err := s.StartCIObservation(ctx, owner.RootThreadID, second); err != nil {
		t.Fatal(err)
	}
	retry, err := selectMonitorArchiveAuthority(ctx, s.Store, r.Workspace, a)
	if err != nil || retry != chosen {
		t.Fatal("lost reply allocated a different archive", retry, err)
	}
	if err := l.BeginArchiveQuiescence(ctx, retry); err != nil {
		t.Fatal(err)
	}
	local, err := l.CompleteArchiveQuiescence(ctx, retry)
	if err != nil || local.State != "quiescent" {
		t.Fatal(local, err)
	}
	emptyTarget := a
	emptyTarget.TargetID = ""
	after, err := selectMonitorArchiveAuthority(ctx, s.Store, r.Workspace, emptyTarget)
	if err != nil || after != chosen {
		t.Fatal("detached binding was forgotten", after, err)
	}
	for _, kind := range []string{"target", "mode", "operation", "receipt", "root"} {
		bad := a
		switch kind {
		case "target":
			bad.TargetID = "changed"
		case "mode":
			bad.Mode = "changed"
		case "operation":
			bad.OperationID = strings.Repeat("9", 64)
		case "receipt":
			bad.ReceiptID = "fake"
		case "root":
			bad.RootThreadID = "replacement"
		}
		if _, err := selectMonitorArchiveAuthority(ctx, s.Store, r.Workspace, bad); err == nil {
			t.Fatal("conflicting binding accepted", kind)
		}
	}
}
func TestMonitorArchivePendingIsTypedWithoutClaimingLiveOwner(t *testing.T) {
	ctx := context.Background()
	s, req, owner, _ := monitorObserverCLI(t)
	r, _ := s.Store.GetObserver(ctx, req.ID)
	claim, err := s.ClaimCIPoll(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := monitoroperation.ArchiveAuthority{Slug: owner.Slug, RootThreadID: owner.RootThreadID, OperationID: strings.Repeat("8", 64), TargetID: "v2-target", Mode: "explicit"}
	l := monitoroperation.Lifecycle{Store: s.Store, AuthorizeArchive: func(context.Context, monitoroperation.ArchiveAuthority) error { return nil }}
	if err := l.BeginArchiveQuiescence(ctx, a); err != nil {
		t.Fatal(err)
	}
	local, err := l.CompleteArchiveQuiescence(ctx, a)
	if err != nil || local.State != "blocked" || !monitorArchivePollPending(ctx, l, a, local) {
		t.Fatal("unpublished join lost typed wait hint", local, err)
	}
	// A missing controller after restart remains blocked, even with pending=true.
	current, _ := s.Store.GetObserver(ctx, r.ID)
	if current.Poll.State != "intent" || current.LocalProof != nil {
		t.Fatal("pending fabricated unsent or joined proof")
	}
	hard := local
	hard.OperationIDs = []string{strings.Repeat("9", 32)}
	if monitorArchivePollPending(ctx, l, a, hard) {
		t.Fatal("hard/unknown operation became pending")
	}
	changed := a
	changed.TargetID = "changed"
	if monitorArchivePollPending(ctx, l, changed, local) {
		t.Fatal("changed target became pending")
	}
	finish := time.Now().UTC().Add(time.Second)
	receipt := *claim.Poll
	receipt.State = "joined"
	receipt.Handle = "exact-owned-call"
	receipt.FinishedAt = &finish
	if _, err := s.CommitCIPoll(ctx, claim, nil, receipt, ""); err != nil {
		t.Fatal(err)
	}
	if monitorArchivePollPending(ctx, l, a, local) {
		t.Fatal("joined call remained pending")
	}
}

// Run the actual command in a child so Ruby receives its real exit status,
// inherited descriptor and stdout, without Go's test status on that stream.
func TestMonitorArchiveCLIProcess(t *testing.T) {
	if os.Getenv("MONITOR_ARCHIVE_CLI_CHILD") != "1" {
		t.Skip("CLI child only")
	}
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) && os.Args[i+1] == "monitor-lifecycle" {
			if err := monitorLifecycleCommand(os.Args[i+2:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestMonitorArchiveThreadlessCLIToRealRubyCaller(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "contract.rb")
	const ruby = `require File.join(ENV.fetch('MONITOR_CLI_REPO'), 'test/dev_session_test')
class DevSessionTest
 def test_real_threadless_monitor_cli_contract
  %w[empty native-error observer-error].each do |kind|
   with_monitor_archive_parent(threadless: true) do |runner, workspace, slug, _fake, _admitted, selected|
    actual=DevSession::CommandRunner.new(out: StringIO.new, err: StringIO.new)
    rows=[]
    capture=actual.method(:capture)
    actual.define_singleton_method(:capture) do |argv, **options|
     assert_action=argv[argv.index('monitor-lifecycle')+1]
     raise 'threadless authority round trip' unless assert_action=='require-idle'
     lock=runner.instance_variable_get(:@held_slug_lock)
     raise 'no exclusive parent lock' unless lock && lock.fetch(:mode)==File::LOCK_EX
     File.open(lock.fetch(:path),File::RDWR){|probe| raise 'unheld parent lock' if probe.flock(File::LOCK_SH|File::LOCK_NB)}
     rows << argv
     capture.call(argv,**options)
    end
    runner.instance_variable_set(:@portal_command,[ENV.fetch('MONITOR_CLI_BINARY'),'-test.run=^TestMonitorArchiveCLIProcess$','--'])
    runner.instance_variable_set(:@command_runner,actual)
    directory=File.join(runner.instance_variable_get(:@workspace_state_root),'monitor-operations',Digest::SHA256.hexdigest(workspace))
    path=File.join(directory,kind=='observer-error' ? 'observers' : '', '1'*32+'.json')
    unless kind=='empty'
     FileUtils.mkdir_p(File.dirname(path)); File.write(path,'{'); File.chmod(0600,path)
    end
    if kind=='empty'
     runner.archive(slug,as_is:true,explicit:true)
     assert_equal([nil],selected)
     assert_nil(runner.instance_variable_get(:@archive_progress_operation_id))
     assert_empty(Dir.glob(File.join(directory,'**','*.json')))
    else
     assert_raises(DevSession::Error){runner.archive(slug,as_is:true,explicit:true)}
     assert_empty(selected)
     assert_equal('{',File.read(path))
     assert(File.directory?(File.join(workspace,'work',slug)))
    end
    assert_equal(1,rows.length)
    refute(rows[0].include?('--archive-operation-id'))
   end
  end
 end
end
`
	if err := os.WriteFile(script, []byte(ruby), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("ruby", script, "--name", "test_real_threadless_monitor_cli_contract")
	command.Env = append(os.Environ(), "MONITOR_ARCHIVE_CLI_CHILD=1", "MONITOR_CLI_REPO="+repo, "MONITOR_CLI_BINARY="+executable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("real CLI/Ruby idle contract: %v\n%s", err, output)
	}
}

func TestMonitorThreadlessIdleRefusesOwnedObserverWithoutMutation(t *testing.T) {
	s, req, owner, stateRoot := monitorObserverCLI(t)
	ctx := context.Background()
	manifest := filepath.Join(filepath.Dir(owner.Socket), "work", owner.Slug, "portal.yml")
	if err := os.WriteFile(manifest, []byte("schema: 1\nslug: "+owner.Slug+"\nrepositories: []\nartifacts: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"observing", "intent", "running"} {
		if state == "intent" {
			if _, err := s.ClaimCIPoll(ctx, req.ID); err != nil {
				t.Fatal(err)
			}
		}
		if state == "running" {
			r, _ := s.Store.GetObserver(ctx, req.ID)
			p := *r.Poll
			p.State, p.Handle = "running", "owned-call"
			if _, err := s.CommitCIPoll(ctx, r, nil, p, ""); err != nil {
				t.Fatal(err)
			}
		}
		before, err := s.Store.GetObserver(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The manifest has no root, contradicting this retained ownership.
		if err := monitorLifecycleCommand([]string{"require-idle", "--workspace", before.Workspace, "--user-state-root", stateRoot, "--session-slug", owner.Slug}); err == nil {
			t.Fatal("owned observer passed threadless idle")
		}
		after, err := s.Store.GetObserver(ctx, req.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("idle read mutated owned record", err)
		}
	}
}
