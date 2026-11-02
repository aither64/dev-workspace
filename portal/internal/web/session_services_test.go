package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func servicePublicFixture(workspace string) serviceProjection {
	registry := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	now := "2026-11-02T12:00:00Z"
	return serviceProjection{Schema: 1, Workspace: workspace, Slug: "example", RegistryID: &registry, Registered: true, Count: 1, CountKnown: true, ObservedAt: now,
		Services: []serviceRow{{ID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", Label: "Candidate preview", Phase: "running", State: "unknown", ObservedAt: &now, Error: &serviceDiagnostic{Code: "observation_unavailable", Message: "Service state could not be verified."}}},
		History:  []serviceRow{{ID: "cccccccc-cccc-cccc-cccc-cccccccccccc", Label: "Retired candidate", Phase: "retired", State: "stopped", ObservedAt: &now, RetiredAt: &now}}}
}
func TestServiceProjectionKeepsKnownMembershipSeparateFromObservationAndHistory(t *testing.T) {
	value := servicePublicFixture("/fixture")
	raw, _ := json.Marshal(value)
	result, err := decodeServiceProjection(raw, "/fixture", "example")
	if err != nil || result.Count != 1 || !result.CountKnown || len(result.History) != 1 || result.Services[0].State != "unknown" {
		t.Fatal(result, err)
	}
	value.Services = []serviceRow{}
	value.History = []serviceRow{}
	value.Count = 0
	value.CountKnown = false
	value.Notice = &serviceDiagnostic{Code: "registration_initializing", Message: "Retry the original initialization command."}
	raw, _ = json.Marshal(value)
	result, err = decodeServiceProjection(raw, "/fixture", "example")
	if err != nil || result.CountKnown {
		t.Fatal(result, err)
	}
	value.RegistryID = nil
	value.Registered = false
	value.CountKnown = true
	value.Notice = nil
	raw, _ = json.Marshal(value)
	result, err = decodeServiceProjection(raw, "/fixture", "example")
	if err != nil || result.Count != 0 || !result.CountKnown {
		t.Fatal(result, err)
	}
}
func TestServiceProjectionRejectsForeignPrivateMalformedAndOversizeOutput(t *testing.T) {
	value := servicePublicFixture("/fixture")
	raw, _ := json.Marshal(value)
	for _, bad := range [][]byte{
		[]byte(strings.Replace(string(raw), `"workspace":"/fixture"`, `"workspace":"/other"`, 1)),
		[]byte(strings.Replace(string(raw), `"count":1`, `"count":0`, 1)),
		[]byte(strings.Replace(string(raw), `"phase":"running"`, `"phase":"retired"`, 1)),
		[]byte(strings.Replace(string(raw), `"state":"unknown"`, `"state":"ready"`, 1)),
		[]byte(strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1)),
		[]byte(strings.Replace(string(raw), `"label":"Candidate preview"`, `"label":"Candidate preview","argv":["private"]`, 1)),
		[]byte(string(raw) + ` {}`), []byte(strings.Repeat("x", serviceProjectionMaxBytes+1)),
	} {
		if _, err := decodeServiceProjection(bad, "/fixture", "example"); err == nil {
			t.Fatal("accepted invalid service projection")
		}
	}
}
func TestServiceTemplateEscapesSafeProjectionAndProvidesNoControlsOrLinks(t *testing.T) {
	server := newTestServer(t)
	value := servicePublicFixture(server.config.Workspace)
	value.Services[0].Label = "<script>unsafe</script>"
	data := pageData{Services: &value, ServiceCount: countSnapshot{Value: 1, Known: true}}
	response := httptest.NewRecorder()
	if err := server.templates.ExecuteTemplate(response, "services", data); err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	if !strings.Contains(body, "&lt;script&gt;unsafe&lt;/script&gt;") || !strings.Contains(body, "Retired services (1)") || !strings.Contains(body, "Service state could not be verified.") {
		t.Fatal(body)
	}
	for _, marker := range []string{"<script>", "<button", "<a ", "<input", "argv", "control_group", "workspace-session-"} {
		if strings.Contains(body, marker) {
			t.Fatalf("unexpected %s", marker)
		}
	}
}
func TestServiceCleanupNoticeUsesKnownPhaseProse(t *testing.T) {
	server := newTestServer(t)
	for _, phase := range []string{"services_pending", "services_stopped"} {
		value := servicePublicFixture(server.config.Workspace)
		value.Cleanup = &serviceCleanup{Operation: "archive", OperationID: strings.Repeat("a", 64), Phase: phase}
		response := httptest.NewRecorder()
		if err := server.templates.ExecuteTemplate(response, "services", pageData{Services: &value}); err != nil {
			t.Fatal(err)
		}
		body := response.Body.String()
		if strings.Contains(body, phase) {
			t.Fatal("raw cleanup phase exposed")
		}
		expected := "cleanup is unfinished"
		if phase == "services_stopped" {
			expected = "cleanup is complete"
		}
		if !strings.Contains(body, expected) || !strings.Contains(body, "session's recovery notice") {
			t.Fatal(body)
		}
	}
}

func TestServiceReaderUsesBoundedPublicCommandAndRejectsBusyAndOversize(t *testing.T) {
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	server.config.WorkspaceName = "fixture"
	summary, err := session.Find(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	summary.ObservationIdentity, err = session.ObservationIdentity(summary.Workspace, summary.Slug, filepath.Join(summary.Workspace, summary.Root, summary.Slug), summary.Codex.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	payload := filepath.Join(root, "public.json")
	args := filepath.Join(root, "args")
	command := filepath.Join(root, "dev-session")
	raw, _ := json.Marshal(servicePublicFixture(server.config.Workspace))
	if err := os.WriteFile(payload, raw, 0600); err != nil {
		t.Fatal(err)
	}
	writeCommand := func(script string) {
		t.Helper()
		if err := os.WriteFile(command, []byte("#!/bin/sh\n"+script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeCommand("printf '%s\\n' \"$@\" > '" + args + "'\ncat '" + payload + "'\n")
	server.config.DevSession = command
	status, err := server.sessionServices(context.Background(), summary)
	if err != nil || status.Count != 1 {
		t.Fatal(status, err)
	}
	invocation, err := os.ReadFile(args)
	if err != nil || string(invocation) != "--workspace\nfixture\nservice\nlist\nexample\n--as-is\n--json\n" {
		t.Fatal(string(invocation), err)
	}
	server.services.invalidate()
	writeCommand("printf private-secret >&2\nexit 75\n")
	if status, err := server.sessionServices(context.Background(), summary); err == nil || status != nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal(status, err)
	}
	server.services.invalidate()
	if err := os.WriteFile(payload, []byte(strings.Repeat("x", serviceProjectionMaxBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	writeCommand("cat '" + payload + "'\n")
	if status, err := server.sessionServices(context.Background(), summary); err == nil || status != nil {
		t.Fatal(status, err)
	}
}
func TestServiceReaderRejectsProfileChangeDuringNativeRead(t *testing.T) {
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	summary, err := session.Find(server.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	payload := filepath.Join(root, "public.json")
	command := filepath.Join(root, "dev-session")
	raw, _ := json.Marshal(servicePublicFixture(server.config.Workspace))
	if err := os.WriteFile(payload, raw, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch '" + ready + "'\nwhile [ ! -e '" + release + "' ]; do sleep .01; done\ncat '" + payload + "'\n"
	if err := os.WriteFile(command, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	server.config.DevSession = command
	done := make(chan error, 1)
	go func() { _, err := server.sessionServices(context.Background(), summary); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted superseded profile read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not finish")
	}
}
