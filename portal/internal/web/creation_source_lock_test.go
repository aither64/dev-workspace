package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"golang.org/x/sys/unix"
)

// This is controlled evidence of current accepted-fork refusal and recovery,
// not evidence identifying the holder in an unrelated CI failure. The Go
// Handler and Ruby CLI use their real receipts, source checks and flock path.
func TestAcceptedForkSourceSharedLockRefusalAndReceiptRecovery(t *testing.T) {
	for _, stage := range []int{1, 2} {
		for _, replaceSource := range []bool{false, true} {
			name := fmt.Sprintf("source_ex_%d/replacement=%t", stage, replaceSource)
			t.Run(name, func(t *testing.T) {
				fixture := newAcceptedForkFixture(t, stage)
				var holder *acceptedForkHolder
				if stage == 1 {
					holder = fixture.holdSource(t)
				}
				accepted := fixture.accept(t)
				barrier := fixture.sourceBarrier(t)
				accepted = fixture.receipt(t)
				if !accepted.Validated || accepted.State != "running" {
					t.Fatal("source EX observer did not follow real Go validation")
				}
				if barrier.event.Stage != stage || barrier.event.Device != fixture.device || barrier.event.Inode != fixture.inode {
					t.Fatalf("wrong real source lock: %+v", barrier.event)
				}
				if holder == nil {
					holder = fixture.holdSource(t)
				}
				if barrier.event.PID == holder.command.Process.Pid || barrier.event.PID == os.Getpid() {
					t.Fatal("holder and initializer must be separate processes")
				}
				fixture.assertNoNativeEffects(t)
				bindingPath := fixture.server.creationEvidencePath(accepted) + ".request"
				var binding []byte
				if stage == 1 {
					assertAcceptedForkAbsent(t, bindingPath)
				} else {
					binding = acceptedForkRead(t, bindingPath)
					if ok, err := fixture.server.readCreationBinding(accepted); err != nil || !ok {
						t.Fatalf("second source EX has no exact binding: %t %v", ok, err)
					}
					fixture.assertDestinationUnlocked(t, accepted.Request.Slug)
				}
				// The socket acknowledgement permits super -> real EX|NB while
				// the separate Ruby process still owns SH on this device/inode.
				barrier.release(t)
				fixture.wait(t)
				failed := fixture.receipt(t)
				if failed.State != "failed" || failed.Attempt != 1 || !failed.Validated ||
					!strings.Contains(failed.Error, "another dev-session command is changing source") {
					t.Fatalf("expected exact accepted source-lock refusal: state=%s attempt=%d validated=%t error=%q events=%s",
						failed.State, failed.Attempt, failed.Validated, failed.Error, fixture.events(t))
				}
				t.Logf("accepted receipt state=%s attempt=%d validated=%t", failed.State, failed.Attempt, failed.Validated)
				fixture.assertSameRequest(t, accepted, failed)
				fixture.assertSourceStages(t, stage)
				fixture.assertNoNativeEffects(t)
				assertAcceptedForkAbsent(t, fixture.journal(failed.Request.Slug))
				assertAcceptedForkAbsent(t, filepath.Join(fixture.server.config.Workspace, "work", failed.Request.Slug))
				assertAcceptedForkAbsent(t, fixture.server.creationEvidencePath(failed))
				if stage == 1 {
					assertAcceptedForkAbsent(t, bindingPath)
				} else if !bytes.Equal(binding, acceptedForkRead(t, bindingPath)) {
					t.Fatal("source refusal changed the destination request binding")
				}
				holder.release(t)
				fixture.config["pauseStage"] = 0
				fixture.writeConfig(t)
				if replaceSource {
					fixture.replaceSource(t)
					fixture.retry(t, failed, http.StatusAccepted)
					fixture.wait(t)
					rejected := fixture.receipt(t)
					if rejected.State != "failed" || rejected.Attempt != 2 ||
						!strings.Contains(rejected.Error, "source session changed") {
						t.Fatalf("replacement adopted or wrong refusal: state=%s attempt=%d error=%q events=%s",
							rejected.State, rejected.Attempt, rejected.Error, fixture.events(t))
					}
					t.Logf("replacement refusal state=%s attempt=%d", rejected.State, rejected.Attempt)
					fixture.assertSameRequest(t, failed, rejected)
					fixture.assertNoNativeEffects(t)
					assertAcceptedForkAbsent(t, fixture.journal(failed.Request.Slug))
					assertAcceptedForkAbsent(t, fixture.server.creationEvidencePath(failed))
					assertAcceptedForkAbsent(t, filepath.Join(fixture.server.config.Workspace, "work", failed.Request.Slug))
					if stage == 2 && !bytes.Equal(binding, acceptedForkRead(t, bindingPath)) {
						t.Fatal("replacement rebound the original request")
					}
					return
				}
				fixture.retry(t, failed, http.StatusAccepted)
				fixture.wait(t)
				ready := fixture.assertReady(t, failed)
				fixture.assertOneFork(t, ready, 0)
				fixture.assertReplay(t, ready)
			})
		}
	}

	t.Run("journal_bound_recovery_keeps_created_fork_without_source", func(t *testing.T) {
		fixture := newAcceptedForkFixture(t, 0)
		fixture.config["failTerminal"] = true
		fixture.writeConfig(t)
		accepted := fixture.accept(t)
		fixture.wait(t)
		failed := fixture.receipt(t)
		if failed.State != "failed" || !strings.Contains(failed.Error, "fixture terminal interruption") {
			t.Fatalf("missing actual post-fork interruption: state=%s error=%q events=%s", failed.State, failed.Error, fixture.events(t))
		}
		fixture.assertSameRequest(t, accepted, failed)
		journal := acceptedForkRead(t, fixture.journal(failed.Request.Slug))
		var saved map[string]any
		if err := json.Unmarshal(journal, &saved); err != nil || saved["source_thread_id"] != "thread-1" {
			t.Fatal("interrupted real fork did not retain its source-bound journal")
		}
		if fixture.count(t, "native_fork") != 1 {
			t.Fatal("interruption did not follow exactly one native fork")
		}
		// Only disposable source tracking moves. Existing journal recovery must
		// keep its narrower ability to finish a genuinely created fork.
		if err := os.Rename(filepath.Join(fixture.server.config.Workspace, "work", "source"),
			filepath.Join(fixture.server.config.Workspace, "retained-source")); err != nil {
			t.Fatal(err)
		}
		fixture.config["failTerminal"] = false
		fixture.writeConfig(t)
		fixture.retry(t, failed, http.StatusAccepted)
		fixture.wait(t)
		ready := fixture.assertReady(t, failed)
		fixture.assertOneFork(t, ready, 1)
		fixture.assertReplay(t, ready)
	})
}

type acceptedForkEvent struct {
	Kind      string  `json:"kind"`
	PID       int     `json:"pid"`
	Stage     int     `json:"stage"`
	Mode      string  `json:"mode"`
	Device    uint64  `json:"device"`
	Inode     uint64  `json:"inode"`
	Monotonic float64 `json:"monotonic"`
}

type acceptedForkFixture struct {
	server        *Server
	adapter, ruby string
	configPath    string
	config        map[string]any
	listener      *net.UnixListener
	device, inode uint64
}

func newAcceptedForkFixture(t *testing.T, stage int) *acceptedForkFixture {
	t.Helper()
	s := newTestServer(t)
	providerTestCatalog(t, s, "http://127.0.0.1:1")
	// The provider helper adds WorkspaceName after newTestServer construction.
	// Supply the complete disposable constructor tuple so restart exercises the
	// same real authority/package checks as initial admission.
	runtimeRoot := t.TempDir()
	s.config.CodexSocket = filepath.Join(runtimeRoot, "native.sock")
	s.config.RegistrationMarker = filepath.Join(runtimeRoot, "registration.json")
	s.config.PackageRoot = filepath.Join(runtimeRoot, "package")
	metadataDirectory := filepath.Join(s.config.PackageRoot, "share", "dev-workspace")
	if err := os.MkdirAll(metadataDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"schema":1,"activationEnvironmentAliases":["DEV_WORKSPACES"],"routerSocket":"/run/dev-workspaces/router.sock","userNamespace":"dev-workspaces","agent_teams":{"managed":false}}`
	if err := os.WriteFile(filepath.Join(metadataDirectory, "package.json"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()
	validated, err := New(s.config)
	if err != nil {
		t.Fatalf("initial fixture authorities: %v", err)
	}
	s = validated
	t.Cleanup(s.Close)
	prepareInteractiveConversation(t, s, "source")
	manifestPath := filepath.Join(s.config.Workspace, "work", "source", "portal.yml")
	manifest := acceptedForkRead(t, manifestPath)
	manifest = bytes.ReplaceAll(manifest, []byte("/run/dev-workspace-codex/app-server.sock"), []byte(s.config.CodexSocket))
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	s.config.Codex = &creationTestCodex{browserContractCodex: &browserContractCodex{
		transcript: codex.Transcript{ThreadID: "thread-1", Status: "idle", Model: "model-1", ReasoningEffort: "high"},
	}}
	// Fake only the external provider model catalog, through its real launcher.
	bin := filepath.Join(s.config.HostProfile, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	models, _ := s.config.Codex.ListModels(context.Background())
	data, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	host := "#!/bin/sh\n[ \"$*\" = 'provider models test-workspace openai' ] || exit 91\nprintf '%s\\n' " + quote(string(data)) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "workspace-host"), []byte(host), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := filepath.Abs("../../../test/support/accepted_fork_runner.rb")
	if err != nil {
		t.Fatal(err)
	}
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Fatal(err)
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	env, err := exec.LookPath("env")
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths have a small platform limit; this directory contains
	// only this disposable fixture's socket and is always closed before removal.
	socketDir, err := os.MkdirTemp("", "accepted-fork-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "barrier.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	root := t.TempDir()
	fixture := &acceptedForkFixture{server: s, ruby: ruby, adapter: adapter, listener: listener,
		configPath: filepath.Join(root, "config.json"), config: map[string]any{
			"source": "source", "socket": socket, "pauseStage": stage, "failTerminal": false,
			"events": filepath.Join(root, "events.jsonl"), "state": s.config.UserStateRoot,
			"shell": shell, "hostProfile": s.config.HostProfile, "tmuxSocket": filepath.Join(root, "tmux.sock"),
			"nativeExecutable": filepath.Join(root, "codex"), "version": s.config.CodexVersion,
			"absentCatalog": filepath.Join(root, "absent-provider-catalog.json"),
		}}
	fixture.writeConfig(t)
	if err := os.WriteFile(fixture.config["nativeExecutable"].(string), []byte("#!/bin/sh\nexit 92\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The executable receives Go-produced arguments unchanged, after the same
	// fixed runtime options normally supplied by the installed wrapper. No live
	// session identity or runtime environment is inherited by the disposable CLI.
	argv := []string{env, "-i", "PATH=" + os.Getenv("PATH"), "SHELL=" + shell,
		"DEV_WORKSPACE_CREATION_PROGRESS=1", ruby, adapter, fixture.configPath,
		"--workspace", s.config.Workspace, "--authority-dir", s.config.AuthorityDir,
		"--codex-socket", s.config.CodexSocket, "--codex-version", s.config.CodexVersion}
	words := make([]string, len(argv))
	for i, value := range argv {
		words[i] = quote(value)
	}
	wrapper := filepath.Join(root, "dev-session")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+strings.Join(words, " ")+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.config.DevSession = wrapper
	lock, err := session.LockRuntimeShared(s.config.AuthorityDir, "source")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	var stat unix.Stat_t
	if err := unix.Stat(filepath.Join(s.config.AuthorityDir, "source.lock"), &stat); err != nil {
		t.Fatal(err)
	}
	fixture.device, fixture.inode = stat.Dev, stat.Ino
	return fixture
}

func (f *acceptedForkFixture) writeConfig(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *acceptedForkFixture) accept(t *testing.T) creationReceipt {
	t.Helper()
	response := postCreation(t, f.server, "/api/sessions/source/fork",
		`{"name":"fork","creationDate":"2026-10-24","provider":"openai","model":"model-1","reasoningEffort":"high"}`, "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatalf("real Handler fork admission = %d (events=%s)", response.Code, f.events(t))
	}
	receipt := f.receipt(t)
	if receipt.Request.Source != "source" || receipt.Request.SourceThreadID != "thread-1" ||
		receipt.Request.Provider != "openai" || receipt.Attempt != 1 || receipt.ReceiptID == "" {
		t.Fatal("acceptance lost the source/provider/original receipt identity")
	}
	return receipt
}

func (f *acceptedForkFixture) receipt(t *testing.T) creationReceipt {
	t.Helper()
	receipt, ok := f.server.currentCreation("2026-10-24-fork")
	if !ok {
		t.Fatal("accepted creation receipt missing")
	}
	return receipt
}

func (f *acceptedForkFixture) wait(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { f.server.operationWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f.server.cancelOperations()
		<-done // runCreationCommand owns process-group cancellation and drain.
		t.Fatalf("initializer did not finish within bounded fixture handshake; events=%s", f.events(t))
	}
}

func (f *acceptedForkFixture) retry(t *testing.T, receipt creationReceipt, want int) {
	t.Helper()
	response := postCreation(t, f.server, "/api/sessions/"+receipt.Request.Slug+"/creation/retry",
		fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, receipt.ReceiptID, receipt.Attempt), "application/json")
	if response.Code != want {
		t.Fatalf("retry status=%d want=%d receipt attempt=%d events=%s", response.Code, want, receipt.Attempt, f.events(t))
	}
}

func (f *acceptedForkFixture) assertSameRequest(t *testing.T, before, after creationReceipt) {
	t.Helper()
	if before.ReceiptID != after.ReceiptID || before.Workspace != after.Workspace ||
		before.DeletionHistorySHA256 != after.DeletionHistorySHA256 || !reflect.DeepEqual(before.Request, after.Request) {
		t.Fatal("original receipt/request identity changed")
	}
}

func (f *acceptedForkFixture) assertDestinationUnlocked(t *testing.T, slug string) {
	t.Helper()
	creation, err := session.LockCreation(f.server.config.AuthorityDir, slug)
	if err != nil {
		t.Fatalf("destination creation lock held across second source EX: %v", err)
	}
	defer creation.Close()
	runtime, err := session.LockRuntimeExclusive(f.server.config.AuthorityDir, slug)
	if err != nil {
		t.Fatalf("destination runtime lock held across second source EX: %v", err)
	}
	_ = runtime.Close()
}

func (f *acceptedForkFixture) journal(slug string) string {
	return filepath.Join(f.server.config.Workspace, "worktrees", ".locks", slug+".fork.json")
}

func (f *acceptedForkFixture) replaceSource(t *testing.T) {
	t.Helper()
	path := filepath.Join(f.server.config.Workspace, "work", "source", "portal.yml")
	original := acceptedForkRead(t, path)
	changed := bytes.Replace(original, []byte("thread-1"), []byte("thread-replacement"), 1)
	if bytes.Equal(original, changed) {
		t.Fatal("fixture did not change source identity")
	}
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *acceptedForkFixture) assertReady(t *testing.T, previous creationReceipt) creationReceipt {
	t.Helper()
	ready := f.receipt(t)
	if ready.State != "ready" || ready.Attempt != previous.Attempt+1 || ready.Error != "" || ready.Model != "model-1" || ready.Effort != "high" {
		t.Fatalf("retry not ready: state=%s attempt=%d model=%s effort=%s error=%q events=%s",
			ready.State, ready.Attempt, ready.Model, ready.Effort, ready.Error, f.events(t))
	}
	t.Logf("recovered receipt state=%s attempt=%d", ready.State, ready.Attempt)
	f.assertSameRequest(t, previous, ready)
	if err := f.server.proveCreation(ready); err != nil {
		t.Fatalf("real Ruby completion does not prove ready: %v", err)
	}
	if ok, err := f.server.readCreationBinding(ready); err != nil || !ok {
		t.Fatalf("real Ruby binding missing: %t %v", ok, err)
	}
	var proof creationEvidence
	if err := readCreationJSON(f.server.creationEvidencePath(ready), &proof); err != nil ||
		proof.ThreadID != "thread-fork" || proof.SourceThreadID != "thread-1" || proof.ReceiptID != ready.ReceiptID {
		t.Fatal("completion evidence changed source/destination/receipt")
	}
	authority, err := session.LoadRuntimeAuthority(f.server.config.AuthorityDir, ready.Request.Slug, f.server.config.Workspace)
	if err != nil || authority.State != "ready" || authority.CodexThreadID != "thread-fork" {
		t.Fatalf("real Ruby runtime authority missing: %v", err)
	}
	assertAcceptedForkAbsent(t, f.journal(ready.Request.Slug))
	entries, err := os.ReadDir(filepath.Join(f.server.config.Workspace, "work"))
	if err != nil {
		t.Fatal(err)
	}
	destinations := 0
	for _, entry := range entries {
		if entry.Name() != "source" {
			destinations++
			if entry.Name() != ready.Request.Slug {
				t.Fatal("retry created another destination")
			}
		}
	}
	if destinations != 1 {
		t.Fatalf("destination count=%d", destinations)
	}
	return ready
}

func (f *acceptedForkFixture) assertReplay(t *testing.T, ready creationReceipt) {
	t.Helper()
	before := acceptedForkRead(t, f.config["events"].(string))
	proofBefore := acceptedForkRead(t, f.server.creationEvidencePath(ready))
	bindingBefore := acceptedForkRead(t, f.server.creationEvidencePath(ready)+".request")
	f.retry(t, ready, http.StatusAccepted) // duplicate current retry is a no-op.
	stale := ready
	stale.Attempt--
	f.retry(t, stale, http.StatusConflict)
	wrong := ready
	wrong.ReceiptID = strings.Repeat("b", 64)
	f.retry(t, wrong, http.StatusConflict)
	f.server.Close()
	restarted, err := New(f.server.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	f.server = restarted
	response := postCreation(t, restarted, "/api/sessions/"+ready.Request.Slug+"/creation/retry",
		fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, ready.ReceiptID, ready.Attempt), "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart exact retry status=%d", response.Code)
	}
	readback := f.receipt(t)
	if readback.State != "ready" || readback.Attempt != ready.Attempt || restarted.proveCreation(readback) != nil {
		t.Fatal("restart could not read the exact existing completion proof")
	}
	f.assertSameRequest(t, ready, readback)
	if !bytes.Equal(proofBefore, acceptedForkRead(t, restarted.creationEvidencePath(ready))) ||
		!bytes.Equal(bindingBefore, acceptedForkRead(t, restarted.creationEvidencePath(ready)+".request")) {
		t.Fatal("readback changed the exact existing completion or binding bytes")
	}
	if !bytes.Equal(before, acceptedForkRead(t, f.config["events"].(string))) {
		t.Fatal("duplicate/stale/wrong/restart retry invoked the CLI again")
	}
}

func (f *acceptedForkFixture) events(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(f.config["events"].(string))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f *acceptedForkFixture) count(t *testing.T, kind string) int {
	t.Helper()
	count := 0
	for _, line := range bytes.Split(f.events(t), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event acceptedForkEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("invalid fixture event: %v", err)
		}
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func (f *acceptedForkFixture) assertNoNativeEffects(t *testing.T) {
	t.Helper()
	for _, kind := range []string{"native_fork", "native_start", "native_settings", "native_send", "tmux_create", "provider_bind"} {
		if f.count(t, kind) != 0 {
			t.Fatalf("unexpected %s before source admission: events=%s", kind, f.events(t))
		}
	}
}

func (f *acceptedForkFixture) assertSourceStages(t *testing.T, refused int) {
	t.Helper()
	if f.count(t, "source_attempt") != refused || f.count(t, "source_acquired") != refused-1 || f.count(t, "source_refused") != 1 {
		t.Fatalf("wrong controlled source acquisition/refusal: events=%s", f.events(t))
	}
	t.Logf("controlled EX stage=%d lock=%d:%d events=%s", refused, f.device, f.inode, f.events(t))
}

func (f *acceptedForkFixture) assertOneFork(t *testing.T, ready creationReceipt, settings int) {
	t.Helper()
	if f.count(t, "native_fork") != 1 || f.count(t, "tmux_create") != 1 || f.count(t, "native_start") != 0 ||
		f.count(t, "native_send") != 0 || f.count(t, "native_settings") != settings {
		t.Fatalf("wrong native effects after recovery: events=%s", f.events(t))
	}
	for _, line := range bytes.Split(f.events(t), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event["kind"] == "native_fork" && (event["slug"] != ready.Request.Slug || event["sourceThread"] != ready.Request.SourceThreadID || event["model"] != ready.Model || event["effort"] != ready.Effort) {
			t.Fatal("native fork settings/source/destination mismatch")
		}
		if event["kind"] == "provider_bind" && (event["slug"] != ready.Request.Slug || event["provider"] != ready.Request.Provider) {
			t.Fatal("provider binding mismatch")
		}
	}
	if f.count(t, "provider_bind") == 0 {
		t.Fatal("recovered fork never bound its selected provider")
	}
}

type acceptedForkBarrier struct {
	connection *net.UnixConn
	event      acceptedForkEvent
}

func (f *acceptedForkFixture) sourceBarrier(t *testing.T) *acceptedForkBarrier {
	t.Helper()
	if err := f.listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	connection, err := f.listener.AcceptUnix()
	if err != nil {
		t.Fatalf("source stage rendezvous: %v events=%s", err, f.events(t))
	}
	t.Cleanup(func() { _ = connection.Close() })
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	barrier := &acceptedForkBarrier{connection: connection}
	if err := json.NewDecoder(connection).Decode(&barrier.event); err != nil {
		t.Fatalf("source stage frame: %v", err)
	}
	if barrier.event.Kind != "source_attempt" || barrier.event.PID <= 0 || barrier.event.Mode != "EX" {
		t.Fatalf("invalid source stage: %+v", barrier.event)
	}
	return barrier
}

func (b *acceptedForkBarrier) release(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(b.connection, "continue\n"); err != nil {
		t.Fatal(err)
	}
	_ = b.connection.Close()
}

type acceptedForkHolder struct {
	stdin    io.WriteCloser
	events   chan acceptedForkEvent
	done     chan error
	command  *exec.Cmd
	stderr   bytes.Buffer
	released bool
}

func (f *acceptedForkFixture) holdSource(t *testing.T) *acceptedForkHolder {
	t.Helper()
	command := exec.Command(f.ruby, f.adapter, "--hold", filepath.Join(f.server.config.AuthorityDir, "source.lock"),
		fmt.Sprint(f.device), fmt.Sprint(f.inode))
	holder := &acceptedForkHolder{command: command, done: make(chan error, 1), events: make(chan acceptedForkEvent, 2)}
	var err error
	holder.stdin, err = command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = &holder.stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var event acceptedForkEvent
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				holder.done <- fmt.Errorf("holder frame: %w", err)
				return
			}
			holder.events <- event
		}
		waitErr := command.Wait()
		if scanner.Err() != nil {
			waitErr = fmt.Errorf("holder stdout: %w (wait: %v)", scanner.Err(), waitErr)
		}
		holder.done <- waitErr
	}()
	t.Cleanup(func() { holder.release(t) })
	select {
	case event := <-holder.events:
		if event.Kind != "holder_acquired" || event.Device != f.device || event.Inode != f.inode || event.Mode != "SH" || event.PID == os.Getpid() {
			t.Fatalf("wrong controlled holder: %+v", event)
		}
		t.Logf("controlled holder=%+v", event)
	case err := <-holder.done:
		holder.released = true // Already drained; cleanup must not wait a second time.
		_ = holder.stdin.Close()
		t.Fatalf("holder stopped before ready: %v %s", err, holder.stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("holder ready handshake timed out")
	}
	return holder
}

func (h *acceptedForkHolder) release(t *testing.T) {
	t.Helper()
	if h.released {
		return
	}
	h.released = true
	_, _ = io.WriteString(h.stdin, "release\n")
	_ = h.stdin.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("holder release: %v %s", err, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = h.command.Process.Kill()
		err := <-h.done
		t.Errorf("holder release handshake timed out; drained=%v", err)
	}
	select {
	case event := <-h.events:
		if event.Kind != "holder_released" {
			t.Errorf("wrong release acknowledgement: %+v", event)
		}
		t.Logf("controlled holder=%+v", event)
	default:
		t.Error("holder did not acknowledge release")
	}
}

func acceptedForkRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertAcceptedForkAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unexpected fixture state at %s: %v", filepath.Base(path), err)
	}
}
