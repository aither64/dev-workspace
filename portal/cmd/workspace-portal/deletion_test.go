package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
	"github.com/coder/websocket"
)

func TestTeamPrepareDeleteRejectsUnboundFlags(t *testing.T) {
	base := []string{"--user-state-root", t.TempDir(), "--workspace", t.TempDir(), "--session-slug", "one", "--root-thread-id", "root-one"}
	for _, args := range [][]string{
		append([]string{"prepare-delete"}, base...),
		append(append([]string{"require-idle"}, base...), "--check-only"),
		append(append([]string{"archive"}, base...), "--deletion-operation-id", strings.Repeat("a", 64)),
	} {
		if err := teamCommand(args); err == nil {
			t.Fatal("unbound deletion flags passed")
		}
	}
	args := append(append([]string{"prepare-delete"}, base...), "--deletion-operation-id", strings.Repeat("a", 64))
	if err := teamCommand(args); err == nil || !strings.Contains(err.Error(), "live removal journal") {
		t.Fatalf("missing deletion owner accepted: %v", err)
	}
}

// The Ruby fixture executes the actual command in this already compiled test
// binary. No build, live native daemon, model call or installed package is used.
func TestTeamPrepareDeleteCommandProcess(t *testing.T) {
	if os.Getenv("DEV_WORKSPACE_TEST_DELETE_COMMAND") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "--" {
			if err := run(os.Args[index+1:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestTeamPrepareDeleteRubyOwnerOrderingAndPrivateTombstone(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal("Ruby must come from the repository Nix shell")
	}
	workspace, state := t.TempDir(), t.TempDir()
	home := t.TempDir()
	slug, root := "2026-06-06-demo", "11111111-1111-7111-8111-111111111111"
	known := "22222222-2222-7222-8222-222222222222"
	project := "33333333-3333-7333-8333-333333333333"
	unknownProject := "44444444-4444-7444-8444-444444444444"
	cwd := filepath.Join(workspace, "work", slug)
	store, err := teamruntime.NewStore(state, workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(context.Background(), slug, root, true, func(roster *teamruntime.Roster) error {
		roster.Members = []teamruntime.Member{
			{Address: "implementer0", Role: "implementer", Thread: known, Model: "gpt-6-sol", Effort: "high", ProjectID: project, CreateAttempted: true, BootstrapAttempted: true, State: "ready", AddedAt: time.Now().UTC()},
			{Address: "reviewer0", Role: "reviewer", Model: "gpt-6-sol", Effort: "xhigh", ProjectID: unknownProject, CreateAttempted: true, State: "creating", AddedAt: time.Now().UTC()},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var methods []string
	socket := threadCommandAppServer(t, func(connection *websocket.Conn) error {
		defer connection.CloseNow()
		if err := threadCommandHandshake(connection); err != nil {
			return err
		}
		for {
			request, err := threadCommandReadObject(connection)
			if err != nil {
				return nil
			}
			method, _ := request["method"].(string)
			mu.Lock()
			methods = append(methods, method)
			mu.Unlock()
			var result any
			switch method {
			case "thread/read":
				params, _ := request["params"].(map[string]any)
				if params["threadId"] != known {
					return fmt.Errorf("unknown reservation was inspected/adopted: %#v", params)
				}
				result = map[string]any{"thread": map[string]any{"id": known, "cwd": cwd, "projectId": project, "source": "vscode", "path": nil, "status": map[string]any{"type": "idle"}, "turns": []any{}}}
			case "thread/queue/list", "thread/loaded/list", "thread/turns/list":
				result = map[string]any{"data": []any{}}
			case "thread/goal/get":
				result = map[string]any{"goal": nil}
			default:
				return fmt.Errorf("unexpected deletion preflight RPC: %s", method)
			}
			if err := threadCommandWriteObject(connection, map[string]any{"id": request["id"], "result": result}); err != nil {
				return err
			}
		}
	})
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// This fixture keeps root retirement synthetic and asserts all team state
	// before acknowledging it. Prepare-delete itself crosses into the real Go
	// command, including native idle proof for the confirmed member.
	script := `
require File.join(ARGV.fetch(0), 'test', 'dev_session', 'lifecycle_commands_test')
repository, workspace, state, command, socket, home, root = ARGV
ARGV.clear
slug = '2026-06-06-demo'
FileUtils.mkdir_p(File.join(workspace, 'worktrees'))
wrapper = File.join(workspace, 'portal.rb')
calls = File.join(workspace, 'delete-calls')
File.write(wrapper, <<~RUBY)
  require 'json'
  require 'digest'
  workspace = #{workspace.dump}
  slug = #{slug.dump}
  state = #{state.dump}
  journal_path = File.join(workspace, 'worktrees', '.locks', slug + '.removal.json')
  roster_path = File.join(state, 'codex-teams', File.basename(workspace) + '-' + Digest::SHA256.hexdigest(workspace)[0,16], slug + '.json')
  exit 0 if ARGV[0,2] == ['uploads', 'remove-session']
  journal = JSON.parse(File.read(journal_path))
  if ARGV[0,2] == ['team', 'prepare-delete']
    check = ARGV.include?('--check-only')
    abort 'Ruby helper phase mismatch' unless journal.fetch('phase') == (check ? 'validated' : 'thread_retiring')
    abort 'Ruby operation identity mismatch' unless ARGV.fetch(ARGV.index('--deletion-operation-id') + 1) == journal.fetch('operation_id')
    File.open(#{calls.dump}, 'a') { |file| file.puts(check ? 'check validated' : 'apply thread_retiring') }
    ENV['DEV_WORKSPACE_TEST_DELETE_COMMAND'] = '1'
    ENV['DEV_WORKSPACE_CODEX_HOME'] = #{home.dump}
    exec #{command.dump}, '-test.run=^TestTeamPrepareDeleteCommandProcess$', '--', *ARGV
  end
  roster = JSON.parse(File.read(roster_path))
  unknown = roster.fetch('members').find { |member| member.fetch('address') == 'reviewer0' }
  abort 'retirement ran before durable tombstone' unless journal.fetch('phase') == 'thread_retiring' && unknown.fetch('state') == 'removed' && unknown['removedAt'] && unknown.fetch('threadId') == ''
  if ARGV[0,2] == ['team', 'archive']
    File.open(#{calls.dump}, 'a') { |file| file.puts 'archive known' }
    roster.fetch('members').find { |member| member.fetch('address') == 'implementer0' }['state'] = 'archived'
    File.write(roster_path, JSON.generate(roster))
  elsif ARGV[0,2] == ['thread', 'retire']
    abort 'retirement lost selected state root' unless ARGV.include?('--user-state-root') && ARGV.fetch(ARGV.index('--user-state-root') + 1) == state
    abort 'retirement lost workspace owner' unless ARGV.include?('--workspace') && ARGV.fetch(ARGV.index('--workspace') + 1) == workspace
    abort 'root retired before known member' unless roster.fetch('members').first.fetch('state') == 'archived'
    File.open(#{calls.dump}, 'a') { |file| file.puts 'retire root' }
  else
    abort 'unexpected deletion action'
  end
RUBY
fixture = DevSessionTest.new('fixture')
runner = fixture.send(:runner_for, workspace, env: {
  'DEV_WORKSPACES_STATE' => state,
  DevSession::ENV_PORTAL_COMMAND => [RbConfig.ruby, wrapper].shelljoin,
  DevSession::ENV_CODEX_SOCKET => socket
})
runner.ensure_tracking_files(slug)
manifest = runner.send(:ensure_portal_manifest, slug)
manifest['schema'] = 2
manifest['codex'] = {'thread_id' => root}
manifest['creation'] = {'state' => 'creating', 'initial_goal_attempted' => false, 'initial_goal_sent' => false}
runner.send(:write_portal_manifest, slug, manifest)
runner.define_singleton_method(:quiesce_native_client!) do |*|
  journal = send(:load_removal_journal, slug)
  raise 'quiesce phase mismatch' unless journal.fetch('phase') == 'validated'
  File.open(calls, 'a') { |file| file.puts 'quiesce validated' }
  nil
end
runner.delete(slug, as_is: true, force: false)
STDOUT.flush
exit! 0 # The Go test owns assertions; do not run imported Ruby fixture tests.
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, ruby, "-e", script, repository, workspace, state, command, socket, home, root)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("Ruby/Go deletion fixture: %v\n%s", err, output)
	}
	calls, err := os.ReadFile(filepath.Join(workspace, "delete-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "check validated\nquiesce validated\napply thread_retiring\narchive known\nretire root\n" {
		t.Fatalf("wrong owner ordering: %s", calls)
	}
	paths, err := filepath.Glob(filepath.Join(userstate.WorkspaceDirectory(state, "removed", workspace), "*", "team.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("private tombstone missing: %v %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var retained teamruntime.Roster
	if err := json.Unmarshal(data, &retained); err != nil {
		t.Fatal(err)
	}
	unknown := retained.Members[1]
	if unknown.State != "removed" || unknown.RemovedAt == nil || unknown.ProjectID != unknownProject || !unknown.CreateAttempted || unknown.BootstrapAttempted || unknown.Thread != "" || unknown.Model != "gpt-6-sol" || unknown.Effort != "xhigh" {
		t.Fatalf("private tombstone lost unknown identity/settings: %#v", unknown)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) == 0 {
		t.Fatal("confirmed member never received native idle proof")
	}
}

func TestPiDeletionUploadTailUsesRealOwnerAndOriginalRetry(t *testing.T) {
	ctx := context.Background()
	workspace, state := filepath.Join(t.TempDir(), "workspace"), t.TempDir()
	slug := "2026-06-06-pi-lifecycle"
	store, err := uploads.ForWorkspace(workspace, state)
	if err != nil {
		t.Fatal(err)
	}
	store.MinFreeBytes = 0
	epoch, err := session.CompletedRemovalHistory(workspace, slug, state)
	if err != nil {
		t.Fatal(err)
	}
	owner := func(member, incarnation string) string {
		data, _ := json.Marshal([]string{"pi-workflow-v1", "pi", workspace, slug, strings.Repeat("a", 64), member, incarnation})
		return fmt.Sprintf("pi:pi-workflow-v1:%x", sha256.Sum256(data))
	}
	type retainedFile struct {
		backend *uploads.Backend
		id      string
		removed bool
	}
	files := []retainedFile{}
	add := func(scope uploads.Scope, removed bool) *uploads.Backend {
		t.Helper()
		b := &uploads.Backend{Store: store, ScopeID: scope.ID}
		file, err := b.Create(ctx, conversation.UploadRequest{ClientID: fmt.Sprintf("%08x-0000-4000-8000-000000000000", len(files)+1), Name: "input", Size: 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = b.Complete(ctx, file.ID); err != nil {
			t.Fatal(err)
		}
		files = append(files, retainedFile{b, file.ID, removed})
		return b
	}
	for _, x := range []struct {
		key, epoch string
		removed    bool
	}{
		{owner("lead", strings.Repeat("b", 64)), epoch, true},
		{owner("implementer0", strings.Repeat("c", 64)), epoch, true},
		{owner("lead", strings.Repeat("f", 64)), epoch, false},
		{owner("lead", strings.Repeat("b", 64)), "other-epoch", false},
	} {
		scope, err := store.SessionScope(ctx, slug, x.key, x.epoch)
		if err != nil {
			t.Fatal(err)
		}
		add(scope, x.removed)
	}
	draft, err := store.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := add(draft, true)
	wire, err := b.Prepare(ctx, "initial", "draft-attempt", "goal", []string{files[len(files)-1].id})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RetainCreations(ctx, map[string]uploads.Scope{wire: {Slug: slug, Epoch: epoch}}); err != nil {
		t.Fatal(err)
	}
	binary, _ := os.Executable()
	repository, _ := filepath.Abs("../../..")
	script := `
require File.join(ARGV[0], 'test/dev_session/lifecycle_commands_test')
require File.join(ARGV[0], 'test/support/pi_creation_fixture')
repository, workspace, state, command = ARGV
ARGV.clear
%w[repos work worktrees].each { |path| FileUtils.mkdir_p(File.join(workspace, path)) }
fixture = DevSessionTest.new('fixture').extend(PiCreationFixture)
f = fixture.send(:pi_lifecycle_fixture, workspace, fork: false, state_root: state)
runner, slug = f.values_at(:runner, :slug)
operation = '8' * 64
cleanup = runner.method(:remove_session_uploads!)
runner.define_singleton_method(:remove_session_uploads!) { |*| raise DevSession::Error, 'interrupted upload tail' }
fixture.assert_raises(DevSession::Error) { runner.delete(slug, as_is: true, force: true, operation_id: operation) }
journal_path = runner.send(:lifecycle_journal_file, slug, 'delete')
journal = JSON.parse(File.read(journal_path))
fixture.assert_equal('tracking_committed', journal.fetch('phase'))
fixture.refute(File.exist?(File.join(workspace, 'work', slug)))
native_before = fixture.send(:pi_lifecycle_calls, f)
wrapper = File.join(workspace, 'actual-upload-owner.rb')
File.write(wrapper, "ENV['DEV_WORKSPACE_TEST_DELETE_COMMAND']='1'; exec #{command.dump}, '-test.run=^TestTeamPrepareDeleteCommandProcess$', '--', *ARGV\n")
runner.instance_variable_set(:@portal_command, [RbConfig.ruby, wrapper])
runner.define_singleton_method(:remove_session_uploads!) { |*args| cleanup.call(*args) }
runner.define_singleton_method(:pi_lifecycle_command) { |*| raise 'native lifecycle call after tracking_committed' }
runner.delete(slug, as_is: true, force: true, operation_id: operation)
fixture.refute(File.exist?(journal_path))
completed = JSON.parse(File.read(File.join(journal.fetch('recovery'), 'recovery.json')))
fixture.assert_equal('removed', completed.fetch('phase'))
fixture.assert_equal(operation, completed.fetch('operation_id'))
fixture.assert_equal(journal.fetch('pi'), completed.fetch('pi'))
fixture.assert_equal(native_before, fixture.send(:pi_lifecycle_calls, f))
f[:audit].each { |path, bytes| fixture.assert_equal(bytes, File.binread(path)) }
exit! 0
`
	command := exec.Command("ruby", "-e", script, repository, workspace, state, binary)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ordinary Ruby/real Go upload tail: %v\n%s", err, output)
	}
	for _, file := range files {
		content, err := file.backend.Open(ctx, file.id)
		if file.removed && err == nil {
			content.File.Close()
			t.Fatal("owned upload survived original removal")
		}
		if !file.removed {
			if err != nil {
				t.Fatal("foreign upload changed", err)
			}
			content.File.Close()
		}
	}
}
