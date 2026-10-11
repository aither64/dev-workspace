package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlNegotiatesActionsAndUsesRetainedStart(t *testing.T) {
	for _, action := range []string{"start", "stop", "release"} {
		t.Run(action, func(t *testing.T) {
			state := "stopped"
			if action == "stop" {
				state = "running"
			}
			helper := filepath.Join(t.TempDir(), "helper")
			script := `#!/bin/sh
if [ "$DEVCLUSTER_PORTAL_CONTROLS" != 1 ]; then exit 1; fi
if [ "$1" = status ]; then
  printf '%s' '{"schema":2,"found":true,"kind":"alpha","state":"` + state + `","ready":false,"services":[],"actions":["` + action + `"]}'
else
  printf '%s\n' "$@" > "$0.invocation"
fi
`
			if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			runner := Runner{Workspace: t.TempDir(), Providers: testProviders("alpha", helper, "Alpha")}
			if err := runner.Control(context.Background(), "alpha", "example", action); err != nil {
				t.Fatal(err)
			}
			invocation, err := os.ReadFile(helper + ".invocation")
			if err != nil {
				t.Fatal(err)
			}
			want := action + "\nexample\n"
			if action == "start" {
				want += "--existing\n"
			}
			if action == "release" {
				want = "reset\nexample\n"
			}
			if string(invocation) != want {
				t.Fatalf("invocation %q, want %q", invocation, want)
			}
		})
	}
}

func TestControlRefusesAbsentUnsupportedBusyAndInvalidActions(t *testing.T) {
	for _, payload := range []string{
		`{"schema":2,"found":false,"kind":"alpha"}`,
		`{"schema":2,"found":true,"kind":"alpha","state":"stopped","ready":false,"services":[]}`,
		`{"schema":2,"found":true,"kind":"alpha","state":"stopped","ready":false,"services":[],"actions":[]}`,
		`{"schema":2,"found":true,"kind":"alpha","state":"stopped","ready":false,"services":[],"actions":["arbitrary"]}`,
		`{"schema":2,"found":true,"kind":"alpha","state":"running","ready":false,"services":[],"actions":["start"]}`,
		`{"schema":2,"found":true,"kind":"alpha","state":"stopped","ready":false,"services":[],"actions":["start","start"]}`,
	} {
		helper := statusHelper(t, payload)
		runner := Runner{Workspace: t.TempDir(), Providers: testProviders("alpha", helper, "Alpha")}
		if err := runner.Control(context.Background(), "alpha", "example", "start"); err == nil {
			t.Fatalf("accepted %s", payload)
		}
		invocation, _ := os.ReadFile(helper + ".arguments")
		if string(invocation) != "status\nexample\n--json\n" {
			t.Fatalf("ran mutation: %q", invocation)
		}
	}
	cache := &StatusCache{}
	cache.observe("example\x00alpha", Status{Kind: "alpha", State: "stopped", Actions: []string{"start"}}, true, "")
	helper := filepath.Join(t.TempDir(), "busy")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 75\n"), 0755); err != nil {
		t.Fatal(err)
	}
	runner := Runner{Workspace: t.TempDir(), Cache: cache, Providers: testProviders("alpha", helper, "Alpha")}
	if err := runner.Control(context.Background(), "alpha", "example", "start"); err == nil || !strings.Contains(err.Error(), "changing") {
		t.Fatalf("busy result: %v", err)
	}
	statuses, err := runner.Inspect("example")
	if err != nil || len(statuses) != 1 || statuses[0].CanControl("start") {
		t.Fatalf("enabled cached controls: %#v, %v", statuses, err)
	}
}
