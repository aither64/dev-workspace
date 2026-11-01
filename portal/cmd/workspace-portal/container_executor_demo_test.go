//go:build container_executor_demo

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

// This is deliberately an opt-in fixture, not a production environment manager.
// Only the Responses backend is synthetic; all tool dispatch is stock native.
func TestContainerExecutorDemo(t *testing.T) {
	if os.Getenv("CONTAINER_EXECUTOR_DEMO") != "1" {
		t.Skip("set CONTAINER_EXECUTOR_DEMO=1; see docs/container-executor-demo.md")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	native := demoExecutable(t, "DEMO_CODEX")
	bash := demoExecutable(t, "DEMO_BASH")
	git := demoExecutable(t, "DEMO_GIT")
	nix := demoExecutable(t, "DEMO_NIX")
	podman := demoExecutable(t, "DEMO_PODMAN")
	helpers := os.Getenv("DEMO_PODMAN_HELPERS")
	for _, name := range []string{"crun", "conmon"} {
		if info, err := os.Stat(filepath.Join(helpers, name)); err != nil || info.Mode()&0111 == 0 {
			t.Fatalf("DEMO_PODMAN_HELPERS missing executable %s", name)
		}
	}
	evidence := os.Getenv("CONTAINER_EXECUTOR_DEMO_LOG_DIR")
	if !filepath.IsAbs(evidence) {
		t.Fatal("CONTAINER_EXECUTOR_DEMO_LOG_DIR must be an owned absolute evidence directory")
	}
	demoMkdir(t, evidence)
	logDir, err := os.MkdirTemp(evidence, "container-executor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence: %s", logDir)
	// Nix shells may retire TMPDIR on exit. Keep uncertain fixture recovery
	// independent of that wrapper's lifetime.
	root, err := os.MkdirTemp("/tmp", "container-executor-demo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("owned fixture root: %s", root)
	// Remove only the exact directory this invocation created, after owned
	// processes and containers have been joined by later cleanup handlers.
	cleanupSafe := true
	t.Cleanup(func() {
		if cleanupSafe {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("fixture root removal failed; retained path %s: %v", root, err)
			}
		} else {
			t.Errorf("retained fixture with uncertain process/container cleanup: %s", root)
		}
	})
	home := filepath.Join(root, "host-home")
	codexHome := filepath.Join(home, ".codex")
	demoMkdir(t, codexHome)
	path := strings.Join([]string{filepath.Dir(bash), filepath.Dir(git), filepath.Dir(nix), helpers, "/run/current-system/sw/bin"}, ":")
	env := []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "PATH=" + path,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_DATA_HOME=" + filepath.Join(home, ".local/share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime"),
		"TMPDIR=" + filepath.Join(root, "tmp"), "LANG=C.UTF-8", "OPENAI_API_KEY=fixture-only"}
	demoMkdir(t, filepath.Join(root, "runtime"))
	demoMkdir(t, filepath.Join(root, "tmp"))
	version := demoCommand(t, ctx, env, native, "--version")
	if strings.TrimSpace(version) != "codex-cli 0.165.0" {
		t.Fatalf("requires exact native 0.165.0, got %q", version)
	}
	t.Logf("native: %s (%s)", strings.TrimSpace(version), native)
	uid := strconv.Itoa(os.Getuid())
	if uid == "0" {
		t.Fatal("demo requires a rootless operator")
	}
	baseArgs := []string{"--root", filepath.Join(root, "podman-store"), "--runroot", filepath.Join(root, "podman-run"),
		"--tmpdir", filepath.Join(root, "podman-tmp"), "--storage-driver=vfs", "--events-backend=file",
		"--runtime", filepath.Join(helpers, "crun"), "--conmon", filepath.Join(helpers, "conmon")}
	pm := func(c context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(c, podman, append(append([]string{}, baseArgs...), args...)...)
		cmd.Env = env
		return cmd
	}
	// Register before creating containers; early setup failures still tear down.
	var ownedNames []string
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, name := range ownedNames {
			output, err := pm(cleanup, "rm", "--force", "--ignore", name).CombinedOutput()
			if err != nil {
				cleanupSafe = false
				t.Errorf("owned container cleanup %s: %v %s", name, err, output)
			}
		}
		if len(ownedNames) != 0 {
			var stderr bytes.Buffer
			list := pm(cleanup, "ps", "--all", "--format", "{{.Names}}")
			list.Stderr = &stderr
			output, err := list.Output()
			if err != nil || len(bytes.TrimSpace(output)) != 0 {
				cleanupSafe = false
				t.Errorf("private fixture containers remain: %v stdout=%s stderr=%s", err, output, stderr.String())
			} else if stderr.Len() != 0 {
				t.Logf("private Podman cleanup warnings: %s", stderr.String())
			}
		}
		// Native Podman may retain a pause process for its private rootless
		// namespace even after the last container exits. This command is scoped
		// to the fixture's HOME/runtime/store and stops that private pause process.
		if output, err := pm(cleanup, "system", "migrate").CombinedOutput(); err != nil {
			cleanupSafe = false
			t.Errorf("private Podman namespace cleanup: %v %s", err, output)
		}
	})
	info, err := pm(ctx, "info", "--format", "{{.Host.Security.Rootless}}").Output()
	if err != nil || strings.TrimSpace(string(info)) != "true" {
		t.Fatalf("private rootless Podman preflight: %v %s", err, info)
	}
	shared := filepath.Join(root, "shared.git")
	seed := filepath.Join(root, "seed")
	demoMkdir(t, seed)
	demoCommand(t, ctx, env, git, "init", seed)
	demoWrite(t, filepath.Join(seed, "tracked.txt"), []byte("fixture seed\n"))
	demoCommand(t, ctx, env, git, "-C", seed, "add", "tracked.txt")
	seedMessage := filepath.Join(root, "seed-commit-message")
	demoWrite(t, seedMessage, []byte("fixture: seed disposable shared Git repository\n\nKeep shared-object assertions separate from real development repositories.\n"))
	demoCommand(t, ctx, env, git, "-C", seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-F", seedMessage)
	demoCommand(t, ctx, env, git, "clone", "--bare", seed, shared)
	target := filepath.Join(root, "same-path")
	demoMkdir(t, target)
	demoWrite(t, filepath.Join(target, "marker"), []byte("host\n"))
	demoWrite(t, filepath.Join(target, "patch.txt"), []byte("host\n"))
	demoPNG(t, filepath.Join(target, "image.png"), color.RGBA{G: 255, A: 255})
	hostImage, _ := os.ReadFile(filepath.Join(target, "image.png"))
	type container struct{ id, name, data, worktree string }
	var containers []container
	var environments strings.Builder
	environments.WriteString("default = \"none\"\ninclude_local = false\n")
	for i, id := range []string{"alpha", "beta"} {
		c := container{id: id, name: filepath.Base(root) + "-" + id, data: filepath.Join(root, id, "data"), worktree: filepath.Join(root, "worktree-"+id)}
		containers = append(containers, c)
		ownedNames = append(ownedNames, c.name)
		t.Logf("owned container name: %s (private store %s)", c.name, filepath.Join(root, "podman-store"))
		rootfs := filepath.Join(root, id, "rootfs")
		for _, dir := range []string{"etc", "proc", "sys", "dev", "tmp", "run", "var/tmp", "home/executor", "cache", "nix/store", "nix/var/nix/daemon-socket", strings.TrimPrefix(target, "/"), strings.TrimPrefix(shared, "/"), strings.TrimPrefix(c.worktree, "/")} {
			demoMkdir(t, filepath.Join(rootfs, dir))
		}
		demoWrite(t, filepath.Join(rootfs, "etc/passwd"), []byte("root:x:0:0:Fixture:/home/executor:"+bash+"\n"))
		demoWrite(t, filepath.Join(rootfs, "etc/group"), []byte("root:x:0:\n"))
		demoMkdir(t, filepath.Join(rootfs, "bin"))
		if err := os.Symlink(bash, filepath.Join(rootfs, "bin/sh")); err != nil {
			t.Fatal(err)
		}
		demoMkdir(t, c.data)
		demoWrite(t, filepath.Join(c.data, "marker"), []byte(id+"\n"))
		demoWrite(t, filepath.Join(c.data, "patch.txt"), []byte(id+"\n"))
		pixel := color.RGBA{R: 255, A: 255}
		if i == 1 {
			pixel = color.RGBA{B: 255, A: 255}
		}
		demoPNG(t, filepath.Join(c.data, "image.png"), pixel)
		demoCommand(t, ctx, env, git, "--git-dir="+shared, "worktree", "add", "-b", id, c.worktree)
		args := append(append([]string{}, baseArgs...), "run", "--name", c.name, "--rm", "--pull=never", "--interactive", "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only", "--pids-limit=256", "--memory=1g", "--memory-swap=1g", "--systemd=false",
			"--env=HOME=/home/executor", "--env=CODEX_HOME=/home/executor/.codex", "--env=PATH="+path,
			"--env=SHELL="+bash, "--env=XDG_CACHE_HOME=/cache", "--tmpfs=/tmp:rw,size=32m", "--tmpfs=/run:rw,size=8m", "--tmpfs=/var/tmp:rw,size=8m",
			"--mount=type=bind,src=/nix/store,dst=/nix/store,ro",
			"--mount=type=bind,src=/nix/var/nix/daemon-socket/socket,dst=/nix/var/nix/daemon-socket/socket,ro",
			"--mount=type=bind,src="+c.data+",dst="+target,
			"--mount=type=bind,src="+shared+",dst="+shared,
			"--mount=type=bind,src="+c.worktree+",dst="+c.worktree)
		for _, dir := range []string{"home/executor", "cache"} {
			source := filepath.Join(root, id, dir)
			demoMkdir(t, source)
			args = append(args, "--mount=type=bind,src="+source+",dst=/"+dir)
		}
		demoMkdir(t, filepath.Join(root, id, "home/executor/.codex"))
		args = append(args, "--rootfs", rootfs, native, "exec-server", "--listen", "stdio")
		encoded, _ := json.Marshal(args)
		fmt.Fprintf(&environments, "\n[[environments]]\nid=%s\nprogram=%s\nargs=%s\ninitialize_timeout_sec=20\n", strconv.Quote(id), strconv.Quote(podman), encoded)
	}
	demoWrite(t, filepath.Join(codexHome, "environments.toml"), []byte(environments.String()))
	backend := &demoResponses{plans: map[string]*demoPlan{}}
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	config := "model_provider=\"fixture\"\nmodel=\"gpt-5.5\"\napproval_policy=\"never\"\nsandbox_mode=\"danger-full-access\"\n" +
		"[features]\nshell_snapshot=false\nunified_exec=true\nplugins=false\napps=false\nremote_plugin=false\n" +
		"[model_providers.fixture]\nname=\"Synthetic local fixture\"\nwire_api=\"responses\"\nsupports_websockets=false\nenv_key=\"OPENAI_API_KEY\"\nbase_url=" + strconv.Quote(server.URL+"/v1") + "\n"
	demoWrite(t, filepath.Join(codexHome, "config.toml"), []byte(config))
	log, err := os.Create(filepath.Join(logDir, "native.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	socket := filepath.Join(root, "app.sock")
	daemon := exec.CommandContext(ctx, native, "app-server", "--listen", "unix://"+socket)
	daemon.Dir, daemon.Env = home, env
	daemon.Stdout, daemon.Stderr = log, log
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("owned App Server PID: %d (process group %d)", daemon.Process.Pid, daemon.Process.Pid)
	joined := make(chan error, 1)
	go func() { joined <- daemon.Wait() }()
	client := codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	stopDaemon := sync.OnceFunc(func() {
		client.Close()
		_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGTERM)
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL)
			<-joined
		}
	})
	t.Cleanup(stopDaemon)
	for end := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("native socket not ready; see native.log")
		}
		time.Sleep(20 * time.Millisecond)
	}
	startThread := func(environment string) (string, error) {
		params := map[string]any{"cwd": target, "model": "gpt-5.5", "approvalPolicy": "never", "sandbox": "danger-full-access", "ephemeral": false,
			"environments": []any{map[string]any{"environmentId": environment, "cwd": target, "runtimeWorkspaceRoots": []string{target}}}}
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		c, done := context.WithTimeout(ctx, 30*time.Second)
		defer done()
		err := client.Request(c, "thread/start", params, &result)
		return result.Thread.ID, err
	}
	run := func(thread, label string, item map[string]any, allowFailure bool) string {
		t.Helper()
		plan := &demoPlan{item: item}
		backend.mu.Lock()
		backend.plans[label] = plan
		backend.mu.Unlock()
		c, done := context.WithTimeout(ctx, 15*time.Second)
		defer done()
		var result struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		err := client.Request(c, "turn/start", map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": label}}, "effort": "low"}, &result)
		last := "turn/start not accepted"
		if err == nil {
			if result.Turn.ID == "" {
				t.Fatal("turn/start returned no turn ID")
			}
			// Ordinary persisted fixture threads support the existing idle helper.
			// Their entire home/database is still private disposable fixture data.
			for {
				idleErr := client.RequireThreadIdle(c, thread, target)
				if idleErr == nil {
					last = "idle after accepted turn " + result.Turn.ID
					break
				}
				last = idleErr.Error()
				if c.Err() != nil {
					err = c.Err()
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		backend.mu.Lock()
		output, calls := plan.output, plan.calls
		backend.mu.Unlock()
		if err != nil && !allowFailure {
			t.Fatalf("%s: %v; last=%s; dispatches=%d output=%s; native.log=%s", label, err, last, calls, output, log.Name())
		}
		if !allowFailure && (calls != 1 || output == "") {
			t.Fatalf("%s: wanted one real tool dispatch/output, calls=%d output=%q", label, calls, output)
		}
		if allowFailure && err == nil && (calls != 1 || output == "") {
			t.Fatalf("%s: neither visible rejection/wait nor actual attempted tool output", label)
		}
		t.Logf("%s: dispatches=%d error=%v last=%s output=%s", label, calls, err, last, output)
		return output
	}
	assertFile := func(path, want string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("%s: %v got %q want %q", path, err, data, want)
		}
	}
	threads := map[string]string{}
	namespaces := map[string]string{}
	for _, c := range containers {
		thread, err := startThread(c.id)
		if err != nil || thread == "" {
			t.Fatalf("bind %s: %v; no local fallback", c.id, err)
		}
		threads[c.id] = thread
		inspect := pm(ctx, "inspect", "--format", "{{.State.Pid}}", c.name)
		pidText, err := inspect.Output()
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
		if err != nil || pid <= 0 {
			t.Fatalf("invalid owned container PID %q", pidText)
		}
		mapping, err := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid))
		fields := strings.Fields(string(mapping))
		if err != nil || len(fields) < 3 || fields[0] != "0" || fields[1] != uid {
			t.Fatalf("container root must map to host UID %s: %v %s", uid, err, mapping)
		}
		t.Logf("%s uid_map: %s", c.id, strings.TrimSpace(string(mapping)))
		for _, ns := range []string{"mnt", "pid", "user"} {
			own, _ := os.Readlink("/proc/self/ns/" + ns)
			other, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, ns))
			if err != nil || own == other || other == "" {
				t.Fatalf("container %s lacks separate %s namespace: %v", c.id, ns, err)
			}
			if ns != "user" && namespaces[ns] == other {
				t.Fatalf("containers unexpectedly share %s namespace", ns)
			}
			namespaces[ns] = other
			t.Logf("%s %s namespace: %s", c.id, ns, other)
		}
		command := "set -eu\nIFS= read -r marker < marker\nprintf 'route:%s\\n' \"$marker\"\nprintf '%s\\n' \"$marker\" > command.txt\n" +
			git + " -C " + demoQuote(c.worktree) + " rev-parse --git-common-dir\n" +
			"printf '%s\\n' \"$marker\" > " + demoQuote(filepath.Join(c.worktree, "private.txt")) + "\n" +
			"printf '%s\\n' \"$marker\" | " + git + " --git-dir=" + demoQuote(shared) + " hash-object -w --stdin\n" +
			"test -r " + demoQuote(bash) + "\nif printf forbidden > /nix/store/" + filepath.Base(root) + "; then exit 91; fi\n" +
			nix + " --extra-experimental-features nix-command store ping --store daemon\n"
		out := run(thread, "demo-route-"+c.id, demoFunction("route-"+c.id, "exec_command", map[string]any{"cmd": command, "shell": bash, "login": false, "yield_time_ms": 1000}), false)
		for _, want := range []string{"route:" + c.id, shared, "Trusted"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s missing %q in %s", c.id, want, out)
			}
		}
		assertFile(filepath.Join(c.data, "command.txt"), c.id+"\n")
		assertFile(filepath.Join(c.worktree, "private.txt"), c.id+"\n")
		object := exec.CommandContext(ctx, git, "--git-dir="+shared, "hash-object", "--stdin")
		object.Env, object.Stdin = env, strings.NewReader(c.id+"\n")
		oid, err := object.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := demoCommand(t, ctx, env, git, "--git-dir="+shared, "cat-file", "blob", strings.TrimSpace(string(oid))); got != c.id+"\n" {
			t.Fatalf("shared Git object: %q", got)
		}
		patch := "*** Begin Patch\n*** Update File: " + filepath.Join(target, "patch.txt") + "\n@@\n-" + c.id + "\n+patched-" + c.id + "\n*** End Patch"
		run(thread, "demo-patch-"+c.id, map[string]any{"type": "custom_tool_call", "call_id": "patch-" + c.id, "name": "apply_patch", "input": patch}, false)
		assertFile(filepath.Join(c.data, "patch.txt"), "patched-"+c.id+"\n")
		out = run(thread, "demo-image-"+c.id, demoFunction("image-"+c.id, "view_image", map[string]any{"path": filepath.Join(target, "image.png")}), false)
		if !demoImageMatches(out, c.id) {
			t.Fatalf("view_image did not return %s's actual image", c.id)
		}
		assertFile(filepath.Join(target, "marker"), "host\n")
		assertFile(filepath.Join(target, "patch.txt"), "host\n")
		if c.id == "alpha" {
			assertFile(filepath.Join(containers[1].data, "marker"), "beta\n")
			assertFile(filepath.Join(containers[1].data, "patch.txt"), "beta\n")
			for _, path := range []string{filepath.Join(containers[1].data, "command.txt"), filepath.Join(containers[1].worktree, "private.txt")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("alpha changed sibling path %s", path)
				}
			}
		}
	}
	// Exercise actual unified-exec stdin in one bound environment.
	out := run(threads["alpha"], "demo-stdin-open", demoFunction("stdin-open", "exec_command", map[string]any{"cmd": "IFS= read -r line; printf '%s\\n' \"$line\" > stdin.txt; printf 'stdin-alpha:%s\\n' \"$line\"", "shell": bash, "login": false, "tty": true, "yield_time_ms": 1000}), false)
	match := regexp.MustCompile(`session ID ([0-9]+)`).FindStringSubmatch(out)
	if len(match) != 2 {
		t.Fatalf("native exec did not leave a running stdin session: %s", out)
	}
	sessionID, _ := strconv.Atoi(match[1])
	run(threads["alpha"], "demo-stdin-write", demoFunction("stdin-write", "write_stdin", map[string]any{"session_id": sessionID, "chars": "fixture-input\n", "yield_time_ms": 1000}), false)
	assertFile(filepath.Join(containers[0].data, "stdin.txt"), "fixture-input\n")
	// Selection rejection is acceptable. If accepted, an actual attempted tool
	// must fail/wait without writing anywhere; never substitute another executor.
	if unknown, err := startThread("missing-fixture-environment"); err != nil {
		t.Logf("unknown environment rejected: %v", err)
	} else {
		run(unknown, "demo-unknown", demoFunction("unknown", "exec_command", map[string]any{"cmd": "printf forbidden > " + demoQuote(filepath.Join(target, "forbidden")), "shell": bash, "login": false}), true)
	}
	if output, err := pm(ctx, "rm", "--force", containers[1].name).CombinedOutput(); err != nil {
		t.Fatalf("remove exact owned executor: %v %s", err, output)
	}
	run(threads["beta"], "demo-executor-loss", demoFunction("executor-loss", "exec_command", map[string]any{"cmd": "printf forbidden > " + demoQuote(filepath.Join(target, "forbidden")), "shell": bash, "login": false}), true)
	stopDaemon() // Join before final assertions so no delayed replay can be hidden.
	for _, dir := range []string{target, containers[0].data, containers[1].data} {
		for _, name := range []string{"forbidden"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("unavailable environment executed in %s: %v", dir, err)
			}
		}
	}
	for _, name := range []string{"command.txt", "stdin.txt"} {
		if _, err := os.Stat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("host fallback wrote %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(containers[1].data, "stdin.txt")); !os.IsNotExist(err) {
		t.Fatal("stdin escaped alpha into beta")
	}
	assertFile(filepath.Join(target, "patch.txt"), "host\n")
	for _, c := range containers {
		assertFile(filepath.Join(c.data, "marker"), c.id+"\n")
		assertFile(filepath.Join(c.data, "command.txt"), c.id+"\n")
		assertFile(filepath.Join(c.data, "patch.txt"), "patched-"+c.id+"\n")
	}
	currentImage, _ := os.ReadFile(filepath.Join(target, "image.png"))
	if !bytes.Equal(hostImage, currentImage) {
		t.Fatal("host image sentinel changed")
	}
	t.Log("PASS: two container executors, native command/stdin/patch/image routing, shared Git/store, and unavailable/lost executor without host fallback")
	if os.Getenv("CONTAINER_EXECUTOR_COLD_RESUME") != "1" {
		return
	}
	// The baseline joined the first private App Server. Keep its exact HOME,
	// database and static environment configuration, then resume without a
	// post-resume turn override: automatic work would run before such an override.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	restarted := exec.CommandContext(ctx, native, "app-server", "--listen", "unix://"+socket)
	restarted.Dir, restarted.Env = home, env
	restarted.Stdout, restarted.Stderr = log, log
	restarted.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := restarted.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("owned restarted App Server PID: %d (process group %d)", restarted.Process.Pid, restarted.Process.Pid)
	restartedJoined := make(chan error, 1)
	go func() { restartedJoined <- restarted.Wait() }()
	client = codex.NewWithOptions(socket, codex.ClientOptions{PreserveThreadInstructionsOnResume: true})
	stopRestarted := sync.OnceFunc(func() {
		client.Close()
		_ = syscall.Kill(-restarted.Process.Pid, syscall.SIGTERM)
		select {
		case <-restartedJoined:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-restarted.Process.Pid, syscall.SIGKILL)
			<-restartedJoined
		}
	})
	t.Cleanup(stopRestarted)
	for end := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("restarted native socket not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	coldCtx, coldDone := context.WithTimeout(ctx, 30*time.Second)
	defer coldDone()
	var resumed struct {
		Thread struct {
			ID           string `json:"id"`
			Environments []struct {
				EnvironmentID string `json:"environmentId"`
				CWD           string `json:"cwd"`
			} `json:"environments"`
		} `json:"thread"`
	}
	if err := client.Request(coldCtx, "thread/resume", map[string]any{"threadId": threads["alpha"], "excludeTurns": true}, &resumed); err != nil {
		t.Fatalf("cold resume release gate: %v", err)
	}
	metadata, _ := json.Marshal(resumed)
	t.Logf("cold resume metadata: %s", metadata)
	out = run(threads["alpha"], "demo-cold-resume", demoFunction("cold-resume", "exec_command", map[string]any{
		"cmd":   "set -eu; IFS= read -r marker < marker; printf '%s\\n' \"$marker\" > cold-resume.txt; printf 'cold-route:%s\\n' \"$marker\"",
		"shell": bash, "login": false, "yield_time_ms": 1000,
	}), true)
	stopRestarted()
	writes := map[string]string{}
	exists := map[string]bool{}
	for label, dir := range map[string]string{"host": target, "alpha": containers[0].data, "beta": containers[1].data} {
		data, err := os.ReadFile(filepath.Join(dir, "cold-resume.txt"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		writes[label] = string(data)
		exists[label] = err == nil
	}
	bound := resumed.Thread.ID == threads["alpha"] && len(resumed.Thread.Environments) == 1 && resumed.Thread.Environments[0].EnvironmentID == "alpha" && resumed.Thread.Environments[0].CWD == target
	supported := bound && exists["alpha"] && writes["alpha"] == "alpha\n" && !exists["host"] && !exists["beta"] && strings.Contains(out, "cold-route:alpha")
	gate := "PASS"
	if !supported {
		gate = "FAIL"
	}
	result, _ := json.MarshalIndent(map[string]any{"releaseGate": gate, "supported": supported, "threadId": threads["alpha"], "resumed": resumed,
		"writes": writes, "exists": exists, "toolOutput": out, "native": native, "scope": "cold binding prerequisite; queue/goal/fork/registration expansion blocked on failure"}, "", "  ")
	demoWrite(t, filepath.Join(logDir, "cold-resume-gate.json"), append(result, '\n'))
	if !supported {
		t.Errorf("RELEASE GATE FAIL: cold resume did not restore alpha before execution; exists=%v writes=%v metadata=%s tool=%s", exists, writes, metadata, out)
	}
}

type demoPlan struct {
	item   map[string]any
	calls  int
	output string
}

type demoResponses struct {
	mu    sync.Mutex
	plans map[string]*demoPlan
}

func (d *demoResponses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") || json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&request) != nil {
		http.Error(w, "unexpected synthetic backend request", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	latest := ""
	for _, item := range request.Input {
		if item["role"] == "user" {
			body, _ := json.Marshal(item["content"])
			latest = string(body)
		}
	}
	var label string
	var plan *demoPlan
	for key, candidate := range d.plans {
		if strings.Contains(latest, key) {
			label, plan = key, candidate
			break
		}
	}
	if plan == nil {
		http.Error(w, "unknown fixture plan", http.StatusBadRequest)
		return
	}
	for _, item := range request.Input {
		if item["call_id"] == plan.item["call_id"] && (item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output") {
			body, _ := json.Marshal(item["output"])
			plan.output = string(body)
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(v any) { data, _ := json.Marshal(v); _, _ = fmt.Fprintf(w, "data: %s\n\n", data) }
	event(map[string]any{"type": "response.created", "response": map[string]any{"id": label}})
	if plan.output == "" && plan.calls == 0 {
		plan.calls++
		event(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": plan.item})
	} else {
		event(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": label + "-answer", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "fixture-complete"}}}})
	}
	event(map[string]any{"type": "response.completed", "response": map[string]any{"id": label, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}})
}

func demoFunction(id, name string, args map[string]any) map[string]any {
	data, _ := json.Marshal(args)
	return map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(data)}
}

func demoImageMatches(output, id string) bool {
	var value any
	if json.Unmarshal([]byte(output), &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(v any) bool {
		switch v := v.(type) {
		case []any:
			for _, child := range v {
				if visit(child) {
					return true
				}
			}
		case map[string]any:
			for key, child := range v {
				if key == "image_url" {
					url, _ := child.(string)
					_, encoded, found := strings.Cut(url, ",")
					if !found {
						continue
					}
					data, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil {
						continue
					}
					img, _, err := image.Decode(bytes.NewReader(data))
					if err != nil {
						continue
					}
					r, g, b, _ := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA()
					if (id == "alpha" && r > 60000 && g < 5000 && b < 5000) || (id == "beta" && b > 60000 && r < 5000 && g < 5000) {
						return true
					}
				}
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func demoExecutable(t *testing.T, key string) string {
	t.Helper()
	path := os.Getenv(key)
	info, err := os.Stat(path)
	if !filepath.IsAbs(path) || !strings.HasPrefix(path, "/nix/store/") || err != nil || info.Mode()&0111 == 0 {
		t.Fatalf("%s must select an existing executable in /nix/store", key)
	}
	return path
}

func demoCommand(t *testing.T, ctx context.Context, env []string, program string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, program, args...)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture command %s %v: %v %s", program, args, err, output)
	}
	return string(output)
}

func demoMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
}
func demoWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}
func demoQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func demoPNG(t *testing.T, path string, pixel color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetRGBA(x, y, pixel)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	demoWrite(t, path, output.Bytes())
}
