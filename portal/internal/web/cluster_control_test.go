package web

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/cluster"
)

func TestClusterControlRejectsOverlapAndSurvivesBrowserDisconnect(t *testing.T) {
	server := newTestServer(t)
	summary := prepareInteractiveConversation(t, server, "example")
	helper := filepath.Join(t.TempDir(), "helper")
	script := `#!/bin/sh
if [ "$1" = status ]; then
  printf '%s' '{"schema":2,"kind":"alpha","found":true,"state":"stopped","ready":false,"services":[],"actions":["start"]}'
  exit
fi
touch "$0.started"
while [ ! -f "$0.finish" ]; do sleep .01; done
printf '%s\n' "$@" > "$0.invocation"
`
	if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	server.clusters = cluster.Runner{Workspace: server.config.Workspace, Providers: []cluster.Provider{{Name: "alpha", Label: "Alpha", Helper: helper}}}
	request := httptest.NewRequest("POST", "/api/sessions/example/start-cluster", strings.NewReader(`{"kind":"alpha"}`))
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.controlCluster(response, request, summary, "start")
		done <- response
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(helper + ".started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	response := httptest.NewRecorder()
	server.controlCluster(response, httptest.NewRequest("POST", "/", strings.NewReader(`{"kind":"alpha"}`)), summary, "start")
	if response.Code != 409 || !strings.Contains(response.Body.String(), "changing") {
		t.Fatalf("overlap: %d %s", response.Code, response.Body)
	}
	if err := os.WriteFile(helper+".finish", nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case response = <-done:
		if response.Code != 200 {
			t.Fatalf("disconnected operation: %d %s", response.Code, response.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not finish")
	}
	invocation, err := os.ReadFile(helper + ".invocation")
	if err != nil || string(invocation) != "start\nexample\n--existing\n" {
		t.Fatalf("invocation: %q %v", invocation, err)
	}
}

func TestClusterControlAdmissionRejectsArchivedAndInvalidRequests(t *testing.T) {
	server := newTestServer(t)
	summary := prepareInteractiveConversation(t, server, "example")
	for _, test := range []struct {
		body     string
		archived bool
		code     int
	}{
		{`{"kind":"alpha","command":"reset"}`, false, 400},
		{`{"kind":"unknown"}`, false, 409},
		{`{"kind":"alpha"}`, true, 409},
	} {
		summary.Archived = test.archived
		response := httptest.NewRecorder()
		server.controlCluster(response, httptest.NewRequest("POST", "/", strings.NewReader(test.body)), summary, "start")
		if response.Code != test.code {
			t.Fatalf("body %s: %d %s", test.body, response.Code, response.Body)
		}
	}
}
