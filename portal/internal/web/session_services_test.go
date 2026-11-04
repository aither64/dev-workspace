package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func serviceReaderFixture(t *testing.T) (*Server, *session.Summary, string) {
	t.Helper()
	s := newTestServer(t)
	prepareInteractiveConversation(t, s, "example")
	summary, err := session.Find(s.config.Workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(t.TempDir(), "dev-session")
	s.config.DevSession = command
	return s, summary, command
}

func TestServiceDetailsRenderThreeCurrentRowsWithKnownMembershipAndUnknownUnit(t *testing.T) {
	s, _, command := serviceReaderFixture(t)
	s.config.WorkspaceName = "fixture"
	value := servicePublicFixture(s.config.Workspace)
	first := value.Services[0]
	first.Label, first.State, first.Error = "main", "running", nil
	unknown := value.Services[0]
	unknown.ID, unknown.Label = "dddddddd-dddd-dddd-dddd-dddddddddddd", "candidate"
	exited := first
	exited.ID, exited.Label, exited.State = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "fast", "exited"
	value.Services, value.Count = []serviceRow{first, unknown, exited}, 3
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "public.json")
	if err := os.WriteFile(payload, raw, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$*\" in\n'--workspace fixture service list example --as-is --json') cat '" + payload + "';;\n*) exit 99;;\nesac\n"
	if err := os.WriteFile(command, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/example/details", nil))
	var details struct {
		Known bool          `json:"servicesKnown"`
		Count countSnapshot `json:"serviceCountSnapshot"`
		HTML  string        `json:"servicesHTML"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || !details.Known || !details.Count.Known || details.Count.Stale || details.Count.Value != 3 {
		t.Fatalf("positive projection or membership changed: status=%d known=%v count=%+v", response.Code, details.Known, details.Count)
	}
	if !strings.Contains(details.HTML, `<h2>Temporary services</h2><span>3</span>`) || strings.Count(details.HTML, "data-service-id=") != 4 || !strings.Contains(details.HTML, "Retired services (1)") {
		t.Fatal("retired history changed current count or rows", details.HTML)
	}
	for _, row := range append(value.Services, value.History...) {
		pattern := regexp.MustCompile(`(?s)<article[^>]*data-service-id="` + regexp.QuoteMeta(row.ID) + `"[^>]*>.*?</article>`)
		matches := pattern.FindAllString(details.HTML, -1)
		if len(matches) != 1 || !strings.Contains(matches[0], "<h3>"+row.Label+"</h3>") || !strings.Contains(matches[0], "<dt>Observed state</dt><dd>"+row.State+"</dd>") {
			t.Fatal("public ID/label/state row missing or duplicated", row.ID)
		}
	}
	for _, private := range []string{"workspace-session-", "argv", "control_group", "workingDirectory", "environment"} {
		if strings.Contains(details.HTML, private) {
			t.Fatal("private field exposed", private)
		}
	}
}

func TestServiceReaderFailureDiagnosticsKeepSafeProducerFactsThroughCache(t *testing.T) {
	const secret = "private-service-canary"
	for _, name := range []string{"success", "busy", "start", "projection", "output-limit", "deadline", "identity-before", "cache-closed"} {
		t.Run(name, func(t *testing.T) {
			s, summary, command := serviceReaderFixture(t)
			var logs bytes.Buffer
			s.config.Logger = log.New(&logs, "", 0)
			t.Setenv("SERVICE_SECRET_CANARY", secret)
			payload := filepath.Join(t.TempDir(), "payload")
			raw, _ := json.Marshal(servicePublicFixture(s.config.Workspace))
			script := "cat '" + payload + "'\n"
			stage, outcome, category := "", "", ""
			switch name {
			case "busy":
				script = "printf '" + secret + "' >&2\nexit 75\n"
				stage, outcome, category = "helper", "exit", "dependency_error"
			case "start":
				s.config.DevSession = command + "-" + secret
				stage, outcome, category = "helper", "start", "dependency_error"
			case "projection":
				raw = []byte(secret)
				stage, outcome, category = "projection", "invalid", "dependency_error"
			case "output-limit":
				raw = bytes.Repeat([]byte(secret), serviceProjectionMaxBytes/len(secret)+1)
				stage, outcome, category = "helper", "output_limit", "dependency_error"
			case "deadline":
				script = "printf '" + secret + "' >&2\nexec sleep 30\n"
				stage, outcome, category = "helper", "deadline", "deadline"
			case "identity-before":
				summary.ObservationIdentity = secret
				stage, outcome, category = "identity_before", "unavailable", "dependency_error"
			case "cache-closed":
				s.services.close()
				stage, outcome, category = "cache_wait", "unavailable", "dependency_error"
			}
			if err := os.WriteFile(payload, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(command, []byte("#!/bin/sh\n"+script), 0755); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			s.observeReads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := pageData{Session: summary}
				s.loadServicePage(r.Context(), &data)
				if name == "success" {
					if data.Services == nil || !data.ServiceCount.Known || data.ServiceCount.Value != 1 {
						t.Fatal("known membership changed", data.ServiceCount)
					}
				} else if data.Services != nil || data.ServiceCount.Known {
					t.Fatal("failed dependency became known")
				}
				_ = json.NewEncoder(w).Encode(data.ServiceCount)
			})).ServeHTTP(response, httptest.NewRequest("GET", "/api/sessions/private-service-canary/details", nil))
			s.services.close()
			if response.Code != 200 || !strings.Contains(response.Header().Get("Server-Timing"), "services;dur=") {
				t.Fatal("diagnostics changed public status or omitted service timing")
			}
			text := logs.String()
			if name == "success" {
				if strings.Contains(text, "portal_service_read") {
					t.Fatal("success emitted producer heartbeat", text)
				}
			} else {
				for _, value := range []string{"stage=" + stage, "outcome=" + outcome, "category=" + category} {
					if !strings.Contains(text, value) {
						t.Fatal("lost producer/cache failure fact", value, text)
					}
				}
				if name != "cache-closed" && !regexp.MustCompile(`portal_service_read correlation=r[0-9a-f]{16} stage=[a-z_]+ outcome=[a-z_]+ elapsed_ms=[0-9]+\.[0-9]{2} exit_code=-?[0-9]+ stdout_bytes=[0-9]+ stderr_bytes=[0-9]+`).MatchString(text) {
					t.Fatal("missing bounded numeric producer record", text)
				}
				if name == "busy" && !strings.Contains(text, "exit_code=75 stdout_bytes=0 stderr_bytes=22") {
					t.Fatal("incorrect child exit/byte counts", text)
				}
			}
			if strings.Contains(text+response.Body.String()+response.Header().Get("Server-Timing"), secret) || strings.Contains(text, command) || strings.Contains(text, s.config.Workspace) {
				t.Fatal("private helper data escaped diagnostics", text)
			}
		})
	}
}

func TestServiceReaderDepartedWaiterCannotHideProducerCancellation(t *testing.T) {
	s, summary, command := serviceReaderFixture(t)
	var logs bytes.Buffer
	s.config.Logger = log.New(&logs, "", 0)
	ready := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf private-canary >&2\ntouch '"+ready+"'\nexec sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.observeReads(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = s.sessionServices(r.Context(), summary)
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/sessions/example/details", nil).WithContext(ctx))
		close(done)
	}()
	waitCoordination(t, func() bool { _, err := os.Stat(ready); return err == nil })
	cancel()
	<-done
	s.services.close()
	text := logs.String()
	if !strings.Contains(text, "portal_read correlation=r0000000000000001") || !strings.Contains(text, "category=canceled") || !strings.Contains(text, "portal_service_read correlation=r0000000000000001 stage=helper outcome=canceled") {
		t.Fatal("departed waiter's sampled trace hid actual producer cancellation", text)
	}
	if strings.Contains(text, "private-canary") {
		t.Fatal("stderr escaped after cancellation", text)
	}
}

func TestServiceReaderSharedWaiterKeepsSingleProducerAndSuccess(t *testing.T) {
	s, summary, command := serviceReaderFixture(t)
	root := t.TempDir()
	ready, release, payload := filepath.Join(root, "ready"), filepath.Join(root, "release"), filepath.Join(root, "payload")
	raw, _ := json.Marshal(servicePublicFixture(s.config.Workspace))
	if err := os.WriteFile(payload, raw, 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho start >> '" + ready + "'\nwhile [ ! -e '" + release + "' ]; do sleep .01; done\ncat '" + payload + "'\n"
	if err := os.WriteFile(command, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { _, err := s.sessionServices(first, summary); firstDone <- err }()
	waitCoordination(t, func() bool { _, err := os.Stat(ready); return err == nil })
	secondDone := make(chan *serviceProjection, 1)
	go func() { value, _ := s.sessionServices(context.Background(), summary); secondDone <- value }()
	waitCoordination(t, func() bool { return coordinationWaiters(s.services) == 2 })
	cancel()
	if err := <-firstDone; err != errServicesUnavailable {
		t.Fatal("canceled reader changed public error", err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if value := <-secondDone; value == nil || value.Count != 1 || !value.CountKnown {
		t.Fatal("departed waiter canceled shared producer", value)
	}
	s.services.close()
	starts, _ := os.ReadFile(ready)
	if string(starts) != "start\n" {
		t.Fatal("duplicate producer", string(starts))
	}
}

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
	var logs bytes.Buffer
	server.config.Logger = log.New(&logs, "", 0)
	trace := &readTrace{started: time.Now(), correlation: "r0000000000000001", stages: make(map[string]time.Duration)}
	ctx := context.WithValue(context.Background(), readTraceKey{}, trace)
	done := make(chan error, 1)
	go func() { _, err := server.sessionServices(ctx, summary); done <- err }()
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
	server.services.close()
	if !strings.Contains(logs.String(), "stage=identity_after outcome=unavailable") || strings.Contains(logs.String(), server.config.HostProfile) {
		t.Fatal("identity revalidation lost its safe stage", logs.String())
	}
}
