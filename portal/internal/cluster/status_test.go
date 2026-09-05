package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"schema":1,"found":true,"kind":"vpsadmin","label":"vpsAdmin",
		"state":"stale","ready":true,"topology":"single","network":"local",
		"links":[{"label":"Web UI","url":"https://webui.example.test:10443/"}],
		"commands":[{"label":"services","value":"vpsadmin-devcluster ssh example services"}],
		"credentials":[{"label":"Admin login","value":"test-admin"}]
	}`)
	vpsadminOS := statusHelper(t, `{"schema":1,"found":false,"kind":"vpsadminos","label":"vpsAdminOS"}`)

	statuses, err := (Runner{Workspace: workspace, Vpsadmin: vpsadmin, VpsadminOS: vpsadminOS}).Inspect("example")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Kind != "vpsadmin" || statuses[0].State != "stale" {
		t.Fatalf("statuses = %#v", statuses)
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
		{"state", `{"schema":1,"found":true,"kind":"vpsadmin","label":"vpsAdmin","state":"broken"}`, "invalid status"},
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

func statusHelper(t *testing.T, payload string) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "helper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$0.arguments\"\nprintf '%s' \"$VPSFREE_DEVCLUSTER_WORKSPACE\" > \"$0.workspace\"\nprintf '%s\\n' '" + payload + "'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}
