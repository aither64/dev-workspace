package cluster

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectVpsadminOSCluster(t *testing.T) {
	workspace := t.TempDir()
	slug := "2026-09-04-test"
	directory := filepath.Join(workspace, ".dev-clusters", "vpsadminos", "clusters", slug)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"topology": "dual\n", "network": "bridge\n", "ready": "\n",
		"config.json": `{"topologies":{"dual":["node1","node2"]}}`,
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	statuses, err := (Runner{Workspace: workspace}).Inspect(slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Kind != "vpsadminos" || statuses[0].State != "stale" {
		t.Fatalf("statuses = %#v", statuses)
	}
	if len(statuses[0].Commands) != 2 || statuses[0].Commands[1].Label != "node2" {
		t.Fatalf("commands = %#v", statuses[0].Commands)
	}
}

func TestInspectVpsadminClusterLinksCommandsAndCredentials(t *testing.T) {
	workspace := t.TempDir()
	slug := "2026-09-04-test"
	directory := filepath.Join(workspace, ".dev-clusters", "vpsadmin", "clusters", slug)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"topology": "single\n", "network": "local\n",
		"config.json": `{
			"topologies":{"single":["node1"]},
			"domains":{"webui":"webui.example.test","auth":"auth.example.test"},
			"adminer":{"webAuth":{"username":"adminer","password":"secret"}}
		}`,
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	statuses, err := (Runner{Workspace: workspace}).Inspect(slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || len(statuses[0].Links) != 2 ||
		statuses[0].Links[0].URL != "https://webui.example.test:10443/" {
		t.Fatalf("links = %#v", statuses)
	}
	if len(statuses[0].Commands) != 2 || statuses[0].Commands[1].Label != "node1" {
		t.Fatalf("commands = %#v", statuses[0].Commands)
	}
	if len(statuses[0].Credentials) != 8 || statuses[0].Credentials[7].Value != "secret" {
		t.Fatalf("credentials = %#v", statuses[0].Credentials)
	}
}

func TestInspectRejectsSymlinkedClusterState(t *testing.T) {
	workspace := t.TempDir()
	target := t.TempDir()
	root := filepath.Join(workspace, ".dev-clusters", "vpsadmin", "clusters")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "test")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Workspace: workspace}).Inspect("test"); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestInspectRejectsSymlinkedClusterConfig(t *testing.T) {
	workspace := t.TempDir()
	slug := "2026-09-04-test"
	directory := filepath.Join(workspace, ".dev-clusters", "vpsadminos", "clusters", slug)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "config.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Workspace: workspace}).Inspect(slug); err == nil {
		t.Fatal("expected symlinked config rejection")
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
