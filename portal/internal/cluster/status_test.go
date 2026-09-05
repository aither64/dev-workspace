package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInspectConsumesRealPackagedHelperContracts(t *testing.T) {
	workspace := t.TempDir()
	slug := "2026-09-05-contract"
	for kind, config := range map[string]string{
		"vpsadmin":   `{"topologies":{"single":["node1"]},"seed":{"users":[{"login":"custom","password":"custom-password"}]}}`,
		"vpsadminos": `{"topologies":{"single":["node1"]}}`,
	} {
		directory := filepath.Join(workspace, ".dev-clusters", kind, "clusters", slug)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{
			"topology": "single\n", "network": "bridge\n", "config.json": config,
		} {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{
		Workspace:  workspace,
		Vpsadmin:   filepath.Join(root, "dev-clusters", "vpsadmin", "bin", "devcluster"),
		VpsadminOS: filepath.Join(root, "dev-clusters", "vpsadminos", "bin", "devcluster"),
	}
	statuses, err := runner.Inspect(slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Kind != "vpsadmin" || statuses[1].Kind != "vpsadminos" {
		t.Fatalf("real helper statuses = %#v", statuses)
	}
	if len(statuses[0].Credentials) != 4 || statuses[0].Credentials[2].Value != "custom" {
		t.Fatalf("custom credentials = %#v", statuses[0].Credentials)
	}
}

func TestInspectUsesHelperOwnedStructuredStatus(t *testing.T) {
	workspace := t.TempDir()
	vpsadmin := statusHelper(t, `{
		"schema":1,"found":true,"kind":"vpsadmin",
		"state":"stale","ready":true,"topology":"single","network":"local",
		"links":[{"label":"Web UI","url":"https://webui.example.test:10443/"}],
		"commands":[{"label":"services","value":"vpsadmin-devcluster ssh example services"}],
		"credentials":[{"label":"Admin login","value":"test-admin"}]
	}`)
	vpsadminOS := statusHelper(t, `{"schema":1,"found":false,"kind":"vpsadminos"}`)

	statuses, err := (Runner{Workspace: workspace, Vpsadmin: vpsadmin, VpsadminOS: vpsadminOS}).Inspect("example")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Kind != "vpsadmin" || statuses[0].State != "stale" {
		t.Fatalf("statuses = %#v", statuses)
	}
	if statuses[0].Label != "vpsAdmin" {
		t.Fatalf("status label = %q", statuses[0].Label)
	}
	if len(statuses[0].Links) != 1 || statuses[0].Links[0].URL != "https://webui.example.test:10443/" {
		t.Fatalf("links = %#v", statuses[0].Links)
	}
	if len(statuses[0].Commands) != 1 || len(statuses[0].Credentials) != 1 {
		t.Fatalf("structured status = %#v", statuses[0])
	}

	for _, helper := range []string{vpsadmin, vpsadminOS} {
		data, err := os.ReadFile(helper + ".arguments")
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "status\nexample\n--json\n" {
			t.Fatalf("helper arguments = %q", data)
		}
		data, err = os.ReadFile(helper + ".workspace")
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != workspace {
			t.Fatalf("helper workspace = %q", data)
		}
	}
}

func TestInspectRejectsIncompatibleOrInvalidHelperStatus(t *testing.T) {
	for _, testCase := range []struct {
		name, payload, message string
	}{
		{"schema", `{"schema":2,"found":true,"kind":"vpsadmin"}`, "incompatible"},
		{"kind", `{"schema":1,"found":true,"kind":"vpsadminos"}`, "incompatible"},
		{"state", `{"schema":1,"found":true,"kind":"vpsadmin","state":"broken"}`, "invalid status"},
		{"label", `{"schema":1,"found":true,"kind":"vpsadmin","label":"helper label","state":"stopped"}`, "invalid status"},
		{"not-found-label", `{"schema":1,"found":false,"kind":"vpsadmin","label":"helper label"}`, "invalid status"},
		{"unknown-field", `{"schema":1,"found":false,"kind":"vpsadmin","extra":true}`, "unknown field"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			helper := statusHelper(t, testCase.payload)
			_, err := (Runner{Workspace: t.TempDir(), Vpsadmin: helper}).Inspect("example")
			if err == nil || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("inspection error = %v", err)
			}
		})
	}
}

func TestReleaseUsesConfiguredHelperAndWorkspace(t *testing.T) {
	workspace := t.TempDir()
	output := filepath.Join(t.TempDir(), "release")
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$1\" \"$2\" \"$VPSFREE_DEVCLUSTER_WORKSPACE\" > \"$OUTPUT\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPUT", output)
	runner := Runner{Workspace: workspace, Vpsadmin: helper}
	if err := runner.Release(context.Background(), "vpsadmin", "2026-09-04-test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "reset\n2026-09-04-test\n"+workspace+"\n" {
		t.Fatalf("release invocation = %q", data)
	}
}

func TestReleaseAllInvokesEveryProviderWithoutAStatusSnapshot(t *testing.T) {
	workspace := t.TempDir()
	output := t.TempDir()
	vpsadmin := releaseHelper(t, filepath.Join(output, "vpsadmin"))
	vpsadminOS := releaseHelper(t, filepath.Join(output, "vpsadminos"))
	runner := Runner{Workspace: workspace, Vpsadmin: vpsadmin, VpsadminOS: vpsadminOS}
	if err := runner.ReleaseAll(context.Background(), "2026-09-05-archive"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"vpsadmin", "vpsadminos"} {
		data, err := os.ReadFile(filepath.Join(output, kind))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "reset\n2026-09-05-archive\n"+workspace+"\n" {
			t.Fatalf("%s release invocation = %q", kind, data)
		}
	}
}

func TestReleaseDeadlineKillsAHelperBlockedOnFlock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "held.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	pidFile := filepath.Join(t.TempDir(), "flock.pid")
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nflock \"$LOCK_PATH\" sh -c 'sleep 30' &\n" +
		"printf '%s\\n' \"$!\" > \"$PID_FILE.tmp\"\n" +
		"mv \"$PID_FILE.tmp\" \"$PID_FILE\"\nwait\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCK_PATH", lockPath)
	t.Setenv("PID_FILE", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workspace := t.TempDir()
	result := make(chan error, 1)
	go func() {
		result <- (Runner{Workspace: workspace, Vpsadmin: helper}).Release(ctx, "vpsadmin", "example")
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not publish its flock PID")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	cancel()
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("release did not return after cancellation")
	}
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("release result = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("release exceeded its bounded cancellation window: %s", elapsed)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	exitDeadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(exitDeadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("flock descendant %d survived cancellation", pid)
	}
}

func releaseHelper(t *testing.T, output string) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$1\" \"$2\" \"$VPSFREE_DEVCLUSTER_WORKSPACE\" > \"" + output + "\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}

func statusHelper(t *testing.T, payload string) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.arguments\"\nprintf '%s' \"$VPSFREE_DEVCLUSTER_WORKSPACE\" > \"$0.workspace\"\nprintf '%s\\n' '" + payload + "'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}
