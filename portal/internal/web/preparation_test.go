package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
)

func preparationTestID(number int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", number)
}

func TestSessionPreparationNamingReceivesAcceptedProvider(t *testing.T) {
	for _, selected := range []string{"", "openai", "pool"} {
		t.Run("provider="+selected, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			providerTestCatalog(t, server, "http://127.0.0.1:1")
			got := make(chan string, 1)
			server.config.SessionNamer = func(_ context.Context, input, provider string) (string, error) {
				if input != "Repair naming provider routing" {
					t.Error("accepted naming input changed")
				}
				got <- provider
				return "", errors.New("stop before reservation")
			}
			id := preparationTestID(1)
			response := postPreparation(t, server, id, "Repair naming provider routing", "", url.Values{"provider": {selected}})
			if response.Code != http.StatusAccepted {
				t.Fatalf("admission=%d %s", response.Code, response.Body.String())
			}
			record := awaitPreparation(t, server, id)
			if record.State != "failed" || record.Slug != "" || record.Snapshot.Input.Provider != selected {
				t.Fatalf("provider intent or stopped state changed: %#v", record)
			}
			select {
			case provider := <-got:
				if provider != selected {
					t.Fatalf("naming provider=%q, accepted=%q", provider, selected)
				}
			default:
				t.Fatal("accepted request never reached naming")
			}
		})
	}
}

func TestSessionPreparationBrowserNotificationPreference(t *testing.T) {
	for _, value := range []string{"true", "false", ""} {
		t.Run("preference="+value, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}
			extra := url.Values{}
			if value != "" {
				extra.Set("browserNotificationsEnabled", value)
			}
			id := preparationTestID(1)
			response := postPreparation(t, server, id, "New session request", "", extra)
			if response.Code != http.StatusAccepted {
				t.Fatalf("admission=%d %s", response.Code, response.Body.String())
			}
			server.operationMu.Lock()
			stored := server.preparations[id]
			server.operationMu.Unlock()
			preference := stored.Snapshot.Input.BrowserNotificationsEnabled
			if value == "" {
				if preference != nil {
					t.Fatal("omitted preference must remain absent")
				}
			} else if preference == nil || *preference != (value == "true") {
				t.Fatalf("saved preference=%v, want %s", preference, value)
			}
			if replay := postPreparation(t, server, id, "New session request", "", extra); replay.Code != http.StatusAccepted {
				t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
			}
			changedPreference := "true"
			if value == "true" {
				changedPreference = "false"
			}
			extra.Set("browserNotificationsEnabled", changedPreference)
			if changed := postPreparation(t, server, id, "New session request", "", extra); changed.Code != http.StatusConflict {
				t.Fatalf("changed preference=%d %s", changed.Code, changed.Body.String())
			}
		})
	}
}

func TestSessionPreparationRejectsInvalidNotificationFields(t *testing.T) {
	for _, test := range []struct {
		name, message string
		fields        url.Values
	}{
		{"empty", "invalid browser notification preference", url.Values{"browserNotificationsEnabled": {""}}},
		{"checkbox default", "invalid browser notification preference", url.Values{"browserNotificationsEnabled": {"on"}}},
		{"duplicate", "duplicate session request field", url.Values{"browserNotificationsEnabled": {"true", "false"}}},
		{"unknown field", "unknown session request field", url.Values{"browserNotifications": {"true"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			response := postPreparation(t, server, preparationTestID(1), "New session request", "", test.fields)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.message) {
				t.Fatalf("rejection=%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func postPreparation(t *testing.T, server *Server, id, prompt, name string, extra url.Values) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{"clientRequestId": {id}, "goal": {prompt}, "creation_date": {"2026-10-03"}, "name": {name}}
	for key, value := range extra {
		values[key] = value
	}
	r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(values.Encode()))
	r.Header.Set("Origin", server.config.BaseURL)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}

func awaitPreparation(t *testing.T, server *Server, id string) sessionPreparation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		server.operationMu.Lock()
		record := server.preparations[id]
		server.operationMu.Unlock()
		if record.State != "running" && record.State != "accepting" {
			return record
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("preparation did not finish")
	return sessionPreparation{}
}

func fixturePreparation(t *testing.T, server *Server, number int, state string) sessionPreparation {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	input := preparationInput{RawPrompt: "raw accepted request", Date: "2026-10-03", Attachments: []string{}}
	record := sessionPreparation{Schema: 1, Workspace: server.config.Workspace, RequestID: preparationTestID(number), InputVersion: 1,
		InputDigest: inputPreparationDigest(input), ReceiptID: strings.Repeat("a", 64), Attempt: 1, State: state, Phase: "naming", StartedAt: now, UpdatedAt: now,
		Snapshot: &preparationSnapshot{Input: input, Goal: input.RawPrompt}}
	if state == "accepting" {
		record.Phase, record.Snapshot.Goal = "accepting", ""
	}
	record.SnapshotDigest = preparationDigest(record.Snapshot)
	return record
}

func TestSessionNameValidation(t *testing.T) {
	for _, output := range []string{`{"name":"workspace-notifications"}`, `{"name":"one-two-three"}`, `{"name":"one-two-three-four-five-six"}`} {
		if _, err := parseSessionName(output); err != nil {
			t.Error(err)
		}
	}
	for _, output := range []string{`{"name":"one"}`, `{"name":"one-two-three-four-five-six-seven"}`, `{"name":"` + strings.Repeat("a", 47) + `-b"}`, `{"name":"One-two-three"}`, `{"name":"one--two-three"}`, `{"name":"one-two-three","extra":0}`, `{"name":"one-two-three","name":"four-five-six"}`, `{"name":"one-two-three"} {}`, `null`, `[]`} {
		if _, err := parseSessionName(output); err == nil {
			t.Errorf("accepted %s", output)
		}
	}
	if got := namingPrompt(strings.Repeat("a", 8191) + "žmore"); len(got) != 8191 {
		t.Fatalf("UTF-8 cutoff len=%d", len(got))
	}
	if got := suffixedSessionName("one-two-three-four-five-six-seven-eight-nine-ten", 12); len(got) > 48 || !strings.HasSuffix(got, "-12") {
		t.Fatalf("suffix=%q", got)
	}
}

func TestSessionNameAdapterFailureAndWorkspaceConcurrency(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	for _, test := range []struct {
		output, outcome string
		err             error
	}{
		{output: "not JSON", outcome: "invalid_output"},
		{output: strings.Repeat("x", 257), outcome: "invalid_output"},
		{output: "```json\n{\"name\":\"one-two-three\"}\n```", outcome: "invalid_output"},
		{err: errors.New("utility unavailable"), outcome: "utility_error"},
	} {
		server.config.SessionNamer = func(context.Context, string, string) (string, error) { return test.output, test.err }
		name, outcome, err := server.namePreparation(context.Background(), "Příliš short prompt", "")
		if !errors.Is(err, errSessionNaming) || name != "" || outcome != test.outcome {
			t.Fatalf("failure=%q %q %v", name, outcome, err)
		}
	}
	entered, release := make(chan time.Time, 3), make(chan struct{})
	var active, peak atomic.Int32
	server.config.SessionNamer = func(ctx context.Context, prompt string, _ string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second || len(prompt) > 8192 {
			t.Error("unbounded adapter request")
		}
		count := active.Add(1)
		for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
		}
		defer active.Add(-1)
		entered <- deadline
		select {
		case <-release:
			return `{"name":"one-two-three"}`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	var wg sync.WaitGroup
	for number := 0; number < 3; number++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := server.namePreparation(context.Background(), strings.Repeat("a", 8191)+"ž", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	for number := 0; number < 2; number++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("adapter did not start")
		}
	}
	select {
	case <-entered:
		t.Error("third call bypassed workspace semaphore")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("concurrent adapter calls=%d", peak.Load())
	}
	server.config.SessionNamer = func(context.Context, string, string) (string, error) {
		t.Fatal("attachment-only prompt invoked adapter")
		return "", nil
	}
	if name, outcome, err := server.namePreparation(context.Background(), " \n ", ""); !errors.Is(err, errSessionNaming) || name != "" || outcome != "invalid_output" {
		t.Fatal(name, outcome, err)
	}
}

func TestSessionNameTimeoutIncludesSemaphoreQueue(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.namingSlots <- struct{}{}
	server.namingSlots <- struct{}{}
	server.config.SessionNamer = func(context.Context, string, string) (string, error) {
		t.Fatal("queued timeout invoked adapter")
		return "", nil
	}
	started := time.Now()
	name, outcome, err := server.namePreparation(context.Background(), "queued naming prompt", "")
	if !errors.Is(err, errSessionNaming) || name != "" || outcome != "timeout" {
		t.Fatal(name, outcome, err)
	}
	if elapsed := time.Since(started); elapsed < 29*time.Second || elapsed > 32*time.Second {
		t.Fatalf("queue budget=%s", elapsed)
	}
}

func preparationReadyFile(t *testing.T, server *Server) (uploads.Scope, conversation.Upload) {
	t.Helper()
	server.uploadStore.MinFreeBytes = 0
	scope, err := server.uploadStore.NewDraft(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID}
	file, err := backend.Create(context.Background(), conversation.UploadRequest{ClientID: preparationTestID(999), Name: "input.txt", Size: 0})
	if err != nil {
		t.Fatal(err)
	}
	file, err = backend.Complete(context.Background(), file.ID)
	if err != nil {
		t.Fatal(err)
	}
	return scope, file
}

func TestSessionPreparationPreIntentFailureDoesNotClaimUploads(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.uploadStore.MinFreeBytes = 0
	ctx := context.Background()
	scope, err := server.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID}
	const contents = "unaccepted selected bytes"
	file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(999), Name: "input.txt", Size: int64(len(contents))})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(contents))
	if _, err := backend.Append(ctx, file.ID, 0, hex.EncodeToString(digest[:]), strings.NewReader(contents)); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	unused, err := server.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var intentWrites atomic.Int32
	server.preparationSync = func(directory string) error {
		if directory == server.operationStore.directory {
			intentWrites.Add(1)
			return errors.New("injected pre-intent directory sync")
		}
		return syncPreparationDirectory(directory)
	}
	var calls atomic.Int32
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	now := server.uploadStore.Now().Add(8 * 24 * time.Hour)
	server.uploadStore.Now = func() time.Time { return now }
	id := preparationTestID(1)
	extra := url.Values{"uploadScope": {scope.ID}, "attachmentIds": {file.ID}}
	response := postPreparation(t, server, id, " raw prompt ", "", extra)
	if response.Code != http.StatusConflict || response.Header().Get("Location") != "" || intentWrites.Load() != 1 {
		t.Fatalf("pre-intent failure=%d %s, writes=%d", response.Code, response.Body.String(), intentWrites.Load())
	}
	server.operationMu.Lock()
	_, published := server.preparations[id]
	_, pending := server.preparationWork[id]
	server.operationMu.Unlock()
	if published || pending || calls.Load() != 0 {
		t.Fatal("failed intent published a preparation or started naming")
	}
	if _, err := os.Lstat(server.preparationPath(id, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation intent exists: %v", err)
	}
	encoded, err := os.ReadFile(filepath.Join(server.uploadStore.Directory, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Scopes      map[string]uploads.Scope   `json:"scopes"`
		Submissions map[string]json.RawMessage `json:"submissions"`
	}
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	if _, exists := data.Scopes[unused.ID]; exists {
		t.Fatal("validation did not persist unrelated expired-scope compaction")
	}
	if len(data.Submissions) != 0 || !data.Scopes[scope.ID].Draft {
		t.Fatal("failed intent left orphan upload ownership")
	}
	content, err := backend.Open(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(content.File)
	content.File.Close()
	if err != nil || string(actual) != contents {
		t.Fatalf("selected bytes=%q %v", actual, err)
	}
	if _, err := backend.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(1000), Name: "editable.txt", Size: 0}); err != nil {
		t.Fatal("failed intent locked the upload scope", err)
	}
	if err := backend.Delete(ctx, file.ID, true); err != nil {
		t.Fatal("failed intent locked the selected file", err)
	}
}

func TestSessionPreparationUploadAdmissionIntentRecoveryAndReplay(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	scope, file := preparationReadyFile(t, server)
	entered := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, prompt string, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	id := preparationTestID(1)
	extra := url.Values{"uploadScope": {scope.ID}, "attachmentIds": {file.ID}}
	bad := url.Values{"uploadScope": {scope.ID}, "attachmentIds": {preparationTestID(1000)}}
	if response := postPreparation(t, server, preparationTestID(2), "raw", "", bad); response.Code != 409 {
		t.Fatal(response.Body.String())
	}
	if _, exists := server.preparations[preparationTestID(2)]; exists {
		t.Fatal("invalid files consumed a replay identity")
	}
	response := postPreparation(t, server, id, " raw attachment request ", "", extra)
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	<-entered
	server.Close()
	record, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if record.Snapshot.Input.RawPrompt != " raw attachment request " || !strings.Contains(record.Snapshot.Goal, "input.txt") {
		t.Fatal("input/goal snapshot was not frozen")
	}
	// Recreate the gap after the claim but before running acceptance was saved.
	record.State, record.Phase = "accepting", "accepting"
	snapshot := *record.Snapshot
	snapshot.Goal = ""
	record.Snapshot, record.SnapshotDigest = &snapshot, preparationDigest(&snapshot)
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	status, ok := restarted.preparationStatus(id)
	if !ok || status.State != "paused" || status.ReceiptID != record.ReceiptID {
		t.Fatalf("claim-gap recovery=%#v", status)
	}
	restarted.uploadStore.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if err := restarted.collectUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: restarted.uploadStore, ScopeID: scope.ID}
	if _, err := backend.Status(context.Background(), file.ID); err != nil {
		t.Fatal("claimed file was collected", err)
	}
	// A plain replay never resolves the current upload catalog, even if it is
	// temporarily unavailable; the accepted input/digest already identify it.
	catalog := filepath.Join(restarted.uploadStore.Directory, "catalog.json")
	encoded, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	replay := postPreparation(t, restarted, id, " raw attachment request ", "", extra)
	if replay.Code != 202 {
		t.Fatal("replay read current uploads", replay.Body.String())
	}
	if err := os.WriteFile(catalog, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPreparationFiftyFilesRecoveryAndCompaction(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	ctx := context.Background()
	server.uploadStore.MinFreeBytes = 0
	scope, err := server.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID}
	ids := make([]string, 51)
	for i := range ids {
		file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(1000 + i), Name: fmt.Sprintf("f-%02d.txt", i), Size: 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Complete(ctx, file.ID); err != nil {
			t.Fatal(err)
		}
		ids[i] = file.ID
	}
	if response := postPreparation(t, server, preparationTestID(2), "prompt", "", url.Values{"uploadScope": {scope.ID}, "attachmentIds": ids}); response.Code != 409 {
		t.Fatalf("51-file admission=%d %s", response.Code, response.Body.String())
	}
	entered := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	id, prompt := preparationTestID(1), " raw fifty-file request "
	extra := url.Values{"uploadScope": {scope.ID}, "attachmentIds": ids[:50]}
	response := postPreparation(t, server, id, prompt, "", extra)
	if response.Code != 202 {
		t.Fatalf("50-file admission=%d %s", response.Code, response.Body.String())
	}
	<-entered
	server.Close()
	record, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil || record.Snapshot == nil || !slices.Equal(record.Snapshot.Input.Attachments, ids[:50]) ||
		strings.Count(record.Snapshot.Goal, `"name":`) != 50 {
		t.Fatalf("50-file durable snapshot: %v", err)
	}
	wire, receiptID := record.Snapshot.Goal, record.ReceiptID
	// The full reader, independently of the store, must reject 51 attachments.
	bad := record
	snapshot := *record.Snapshot
	snapshot.Input.Attachments = ids
	bad.Snapshot, bad.InputDigest, bad.SnapshotDigest = &snapshot, inputPreparationDigest(snapshot.Input), preparationDigest(&snapshot)
	if err := validatePreparation(bad, server.config.Workspace); err == nil {
		t.Fatal("preparation reader accepted 51 attachments")
	}
	restarted, err := New(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	current, ok := restarted.preparationStatus(id)
	if !ok || current.State != "paused" || current.RequestID != id || current.ReceiptID != receiptID || current.Attempt != 1 {
		t.Fatalf("50-file recovery=%#v", current)
	}
	status := httptest.NewRecorder()
	restarted.Handler().ServeHTTP(status, httptest.NewRequest("GET", "/api/session-creations/"+id, nil))
	if status.Code != 200 || !strings.Contains(status.Body.String(), receiptID) {
		t.Fatalf("recovered status=%d %s", status.Code, status.Body.String())
	}
	if replay := postPreparation(t, restarted, id, prompt, "", extra); replay.Code != 202 {
		t.Fatalf("identical replay=%d %s", replay.Code, replay.Body.String())
	}
	reordered := append([]string(nil), ids[:50]...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	for _, changed := range []struct {
		prompt string
		ids    []string
	}{{prompt + "changed", ids[:50]}, {prompt, reordered}} {
		if conflict := postPreparation(t, restarted, id, changed.prompt, "", url.Values{"uploadScope": {scope.ID}, "attachmentIds": changed.ids}); conflict.Code != 409 {
			t.Fatalf("changed input=%d %s", conflict.Code, conflict.Body.String())
		}
	}
	restarted.config.SessionNamer = func(context.Context, string, string) (string, error) { return `{"name":"fifty-file-recovery"}`, nil }
	retry := postCreation(t, restarted, "/api/session-creations/"+id+"/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, receiptID), "application/json")
	if retry.Code != 202 {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	result := awaitPreparation(t, restarted, id)
	if result.State != "handed_off" || result.ReceiptID != receiptID || result.Attempt != 2 || result.Snapshot.Goal != wire ||
		!slices.Equal(result.Snapshot.Input.Attachments, ids[:50]) {
		t.Fatalf("retry changed frozen request: %#v", result)
	}
	receipt := awaitCreation(t, restarted, result.Slug)
	if receipt.Goal != wire || receipt.ReceiptID != receiptID {
		t.Fatal("creation handoff changed wire text or receipt")
	}
	receipt.State, receipt.Validated, receipt.Error = "ready", true, ""
	if err := restarted.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	writeCreationProof(t, restarted, receipt)
	if err := restarted.retireCreation(receipt); err != nil {
		t.Fatal(err)
	}
	compact, err := readPreparation(restarted.preparationPath(id, true), restarted.config.Workspace)
	if err != nil || compact.State != "terminal" || compact.Snapshot != nil || compact.Handoff != nil || compact.ReceiptID != receiptID {
		t.Fatalf("50-file terminal compaction=%#v %v", compact, err)
	}
	if _, err := os.Stat(restarted.preparationPath(id, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("full snapshot remains after compaction", err)
	}
}

func TestSessionPreparationCatalogRejectionIsBeforeAcceptance(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	server.installedTeams = &agentteams.Installed{Managed: true, Catalog: &agentteams.Catalog{
		CatalogDigest: strings.Repeat("b", 64), Teams: map[string]agentteams.Team{"solo": {Description: "A lead", TeamDigest: strings.Repeat("c", 64),
			Roles: map[string]agentteams.Role{"team_lead": {Model: "model-1", Effort: "high", Purpose: "lead", Instructions: "Lead instructions."}}}}}}
	scope, file := preparationReadyFile(t, server)
	catalogPath := filepath.Join(server.uploadStore.Directory, "catalog.json")
	before, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	id := preparationTestID(222)
	extra := url.Values{"team": {"removed-team"}, "catalogDigest": {strings.Repeat("a", 64)}, "model": {"model-1"}, "effort": {"high"},
		"sensitive": {"true"}, "browserNotificationsEnabled": {"false"}, "uploadScope": {scope.ID}, "attachmentIds": {file.ID}}
	rejected := postPreparation(t, server, id, " literal request ", "", extra)
	var proof map[string]string
	if err := json.Unmarshal(rejected.Body.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	if rejected.Code != 409 || proof["code"] != "preparation_catalog_stale" || proof["requestId"] != id || proof["catalogDigest"] != strings.Repeat("a", 64) {
		t.Fatalf("rejection=%d %s", rejected.Code, rejected.Body.String())
	}
	if _, exists := server.preparations[id]; exists {
		t.Fatal("rejected request was accepted")
	}
	if _, err := os.Lstat(server.preparationPath(id, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected receipt exists: %v", err)
	}
	after, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected request changed upload selection or ownership")
	}
	extra.Set("catalogDigest", strings.Repeat("b", 64))
	extra.Set("team", "solo")
	accepted := postPreparation(t, server, id, " literal request ", "", extra)
	if accepted.Code != 202 {
		t.Fatalf("fresh catalog=%d %s", accepted.Code, accepted.Body.String())
	}
	server.installedTeams = nil // Previously accepted request remains exact across catalog changes.
	replay := postPreparation(t, server, id, " literal request ", "", extra)
	if replay.Code != 202 {
		t.Fatalf("accepted replay=%d %s", replay.Code, replay.Body.String())
	}
	var first, second preparationStatus
	json.Unmarshal(accepted.Body.Bytes(), &first)
	json.Unmarshal(replay.Body.Bytes(), &second)
	if first.ReceiptID != second.ReceiptID {
		t.Fatal("replay allocated another receipt")
	}
	extra.Set("catalogDigest", strings.Repeat("a", 64))
	conflict := postPreparation(t, server, id, " literal request ", "", extra)
	if strings.Contains(conflict.Body.String(), "preparation_catalog_stale") {
		t.Fatal("accepted identity conflict supplied rejection proof")
	}
}

func TestSessionPreparationAdmissionReplaysBeforeCatalogAndNaming(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, prompt string, _ string) (string, error) {
		if prompt != " \nOriginal prompt\n " {
			t.Errorf("raw naming prompt=%q", prompt)
		}
		entered <- struct{}{}
		select {
		case <-release:
			return `{"name":"original-prompt-work"}`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	server.installedTeams = &agentteams.Installed{Managed: true, Catalog: &agentteams.Catalog{
		CatalogDigest: strings.Repeat("b", 64), Teams: map[string]agentteams.Team{"solo": {Description: "A lead", TeamDigest: strings.Repeat("c", 64),
			Roles: map[string]agentteams.Role{"team_lead": {Model: "model-1", Effort: "high", Purpose: "lead", Instructions: "Lead instructions."}}}}}}
	extra := url.Values{"team": {"solo"}, "catalogDigest": {strings.Repeat("b", 64)}, "model": {"unavailable-explicit"}, "effort": {"low"}}
	id := preparationTestID(1)
	response := postPreparation(t, server, id, " \nOriginal prompt\n ", "", extra)
	if response.Code != 202 {
		t.Fatalf("admission=%d %s", response.Code, response.Body.String())
	}
	var accepted preparationStatus
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.InitialRequest != " \nOriginal prompt\n " || accepted.URL != "/creations/"+id+"/" || accepted.ReceiptID == "" {
		t.Fatalf("status=%#v", accepted)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("naming did not start")
	}
	server.installedTeams = nil // a changed installed catalog cannot redefine replay
	duplicate := postPreparation(t, server, id, " \nOriginal prompt\n ", "", extra)
	if duplicate.Code != 202 {
		t.Fatalf("replay=%d %s", duplicate.Code, duplicate.Body.String())
	}
	var replay preparationStatus
	json.Unmarshal(duplicate.Body.Bytes(), &replay)
	if replay.ReceiptID != accepted.ReceiptID {
		t.Fatal("response loss allocated a new receipt")
	}
	conflict := postPreparation(t, server, id, "Original prompt", "", extra)
	if conflict.Code != 409 {
		t.Fatalf("whitespace conflict=%d", conflict.Code)
	}
	server.operationMu.Lock()
	stored := server.preparations[id]
	server.operationMu.Unlock()
	if stored.Snapshot.Preset.LeadModel != "unavailable-explicit" || stored.Snapshot.Preset.LeadInstructions != "Lead instructions." {
		t.Fatal("settings snapshot changed")
	}
	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest("GET", accepted.URL, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Original prompt") {
		t.Fatal("accepted prompt is not server-rendered")
	}
	close(release)
	result := awaitPreparation(t, server, id)
	if result.Slug != "2026-10-03-original-prompt-work" {
		t.Fatalf("slug=%q", result.Slug)
	}
	receipt := awaitCreation(t, server, result.Slug)
	if receipt.Model != "unavailable-explicit" || receipt.Effort != "low" || receipt.State != "failed" {
		t.Fatalf("unavailable settings were substituted: %#v", receipt)
	}
}

func TestSessionPreparationDistinctIDsCollideWithoutRenaming(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.SessionNamer = func(context.Context, string, string) (string, error) { return `{"name":"same-prompt-work"}`, nil }
	var wg sync.WaitGroup
	for number := 1; number <= 2; number++ {
		wg.Add(1)
		go func(number int) {
			defer wg.Done()
			response := postPreparation(t, server, preparationTestID(number), "same prompt", "", nil)
			if response.Code != 202 {
				t.Errorf("admission=%d", response.Code)
			}
		}(number)
	}
	wg.Wait()
	first, second := awaitPreparation(t, server, preparationTestID(1)), awaitPreparation(t, server, preparationTestID(2))
	if first.Slug == second.Slug || (first.Slug != "2026-10-03-same-prompt-work" && second.Slug != "2026-10-03-same-prompt-work") {
		t.Fatalf("collision: %q %q", first.Slug, second.Slug)
	}
	if first.ReceiptID == second.ReceiptID {
		t.Fatal("distinct requests share a receipt")
	}
	response := postPreparation(t, server, preparationTestID(3), "custom prompt", "same-prompt-work", nil)
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	third := awaitPreparation(t, server, preparationTestID(3))
	if third.State != "failed" || third.Slug != "" {
		t.Fatalf("custom collision was suffixed: %#v", third)
	}
}

func TestSessionPreparationShutdownPausesWithoutFallback(t *testing.T) {
	server := newTestServer(t)
	entered := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	id := preparationTestID(1)
	if response := postPreparation(t, server, id, "original prompt", "", nil); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	<-entered
	server.Close()
	record, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil || record.State != "paused" || record.Base != "" || record.Slug != "" {
		t.Fatalf("shutdown=%#v %v", record, err)
	}
	restarted, err := New(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	status, ok := restarted.preparationStatus(id)
	if !ok || status.State != "paused" || status.ReceiptID != record.ReceiptID {
		t.Fatalf("restart=%#v", status)
	}
}

func TestSessionPreparationSavedBaseAndRetryAttempt(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	var calls atomic.Int32
	server.config.SessionNamer = func(context.Context, string, string) (string, error) {
		calls.Add(1)
		return `{"name":"locked-name-work"}`, nil
	}
	lock, err := session.LockCreation(server.config.AuthorityDir, "2026-10-03-locked-name-work")
	if err != nil {
		t.Fatal(err)
	}
	id := preparationTestID(1)
	if response := postPreparation(t, server, id, "prompt", "", nil); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	record := awaitPreparation(t, server, id)
	if record.State != "failed" || record.Base != "locked-name-work" {
		t.Fatalf("busy candidate=%#v", record)
	}
	lock.Close()
	request := func(attempt int) *httptest.ResponseRecorder {
		return postCreation(t, server, "/api/session-creations/"+id+"/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, record.ReceiptID, attempt), "application/json")
	}
	if response := request(2); response.Code != 409 {
		t.Fatalf("stale retry=%d", response.Code)
	}
	if response := request(1); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	result := awaitPreparation(t, server, id)
	if result.Slug != "2026-10-03-locked-name-work" || calls.Load() != 1 {
		t.Fatalf("retry reran naming: %#v calls=%d", result, calls.Load())
	}
}

func TestSessionPreparationGenerationChangeCannotReserve(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		close(entered)
		select {
		case <-release:
			return `{"name":"new-name-work"}`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	id := preparationTestID(1)
	if response := postPreparation(t, server, id, "prompt", "", nil); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	<-entered
	if err := os.Remove(server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	close(release)
	server.operationWG.Wait()
	record, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil || record.Base != "" || record.Slug != "" {
		t.Fatalf("superseded mutation: %#v %v", record, err)
	}
}

func TestSessionPreparationRecoveryUsesExactReceipt(t *testing.T) {
	for _, gap := range []string{"intent", "base", "reservation", "receipt", "foreign_receipt"} {
		t.Run(gap, func(t *testing.T) {
			server := newTestServer(t)
			record := fixturePreparation(t, server, 1, "running")
			if gap == "intent" {
				record = fixturePreparation(t, server, 1, "accepting")
			}
			if gap != "intent" {
				record.Base = "frozen-name-work"
			}
			if gap == "reservation" || gap == "receipt" || gap == "foreign_receipt" {
				record.Slug = "2026-10-03-frozen-name-work"
				record.Epoch, _ = session.CompletedRemovalHistory(server.config.Workspace, record.Slug, server.config.UserStateRoot)
				request := preparationCreationRequest(record)
				record.Handoff = &request
			}
			if err := server.savePreparation(record); err != nil {
				t.Fatal(err)
			}
			if gap == "receipt" || gap == "foreign_receipt" {
				receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, Request: *record.Handoff, ReceiptID: record.ReceiptID, DeletionHistorySHA256: record.Epoch,
					Attempt: 4, State: "paused", Phase: "Interrupted", StartedAt: record.StartedAt, UpdatedAt: record.UpdatedAt}
				if gap == "foreign_receipt" {
					receipt.ReceiptID = strings.Repeat("b", 64)
				}
				if err := server.saveCreation(receipt); err != nil {
					t.Fatal(err)
				}
			}
			server.Close()
			restarted, err := New(server.config)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			status, ok := restarted.preparationStatus(record.RequestID)
			if !ok || status.ReceiptID != record.ReceiptID {
				t.Fatalf("identity changed: %#v", status)
			}
			if gap == "foreign_receipt" {
				if status.CanonicalURL != "" || status.State != "failed" {
					t.Fatalf("adopted foreign receipt: %#v", status)
				}
				return
			}
			if gap == "receipt" {
				if status.Attempt != 4 || status.State != "paused" {
					t.Fatalf("handoff gap: %#v", status)
				}
				return
			}
			if gap == "intent" {
				if status.State != "paused" {
					t.Fatalf("intent repair: %#v", status)
				}
				return
			}
			body := fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, record.ReceiptID, status.Attempt)
			response := postCreation(t, restarted, "/api/session-creations/"+record.RequestID+"/retry", body, "application/json")
			if response.Code != 202 {
				t.Fatal(response.Body.String())
			}
			result := awaitPreparation(t, restarted, record.RequestID)
			if result.Slug != "2026-10-03-frozen-name-work" {
				t.Fatalf("reservation changed: %#v", result)
			}
		})
	}
}

func TestSessionPreparationCapacityReplayAndStrictReader(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	record := fixturePreparation(t, server, 1, "paused")
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	for number := 2; number <= maxUnfinishedPreparations; number++ {
		server.preparations[preparationTestID(number)] = fixturePreparation(t, server, number, "paused")
	}
	response := postPreparation(t, server, preparationTestID(600), "new", "", nil)
	if response.Code != 409 {
		t.Fatalf("unfinished limit=%d", response.Code)
	}
	replay := postPreparation(t, server, record.RequestID, record.Snapshot.Input.RawPrompt, "", nil)
	if replay.Code != 202 {
		t.Fatalf("replay at limit=%d %s", replay.Code, replay.Body.String())
	}
	for number := 2; number <= maxPreparationMappings; number++ {
		mapping := fixturePreparation(t, server, number, "paused")
		mapping.State = "terminal"
		server.preparations[mapping.RequestID] = mapping
	}
	response = postPreparation(t, server, preparationTestID(10001), "new", "", nil)
	if response.Code != 409 {
		t.Fatalf("mapping limit=%d", response.Code)
	}
	encoded, _ := json.Marshal(record)
	for _, corrupt := range [][]byte{append(encoded[:len(encoded)-1], []byte(`,"extra":1}`)...), []byte(strings.Replace(string(encoded), `"inputDigest":"`, `"inputDigest":"f`, 1))} {
		if err := os.WriteFile(server.preparationPath(record.RequestID, false), corrupt, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readPreparation(server.preparationPath(record.RequestID, false), server.config.Workspace); err == nil {
			t.Fatal("malformed preparation accepted")
		}
	}
}

func TestSessionPreparationCompactionSurvivesReceiptRetirement(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	record := fixturePreparation(t, server, 1, "handed_off")
	record.Base, record.Slug, record.Phase = "recorded-session-work", "2026-10-03-recorded-session-work", "initializing"
	record.Epoch, _ = session.CompletedRemovalHistory(server.config.Workspace, record.Slug, server.config.UserStateRoot)
	request := preparationCreationRequest(record)
	record.Handoff = &request
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, Request: request, ReceiptID: record.ReceiptID, DeletionHistorySHA256: record.Epoch,
		Attempt: 1, State: "ready", Validated: true, Goal: record.Snapshot.Goal, StartedAt: record.StartedAt, UpdatedAt: record.UpdatedAt}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	writeCreationProof(t, server, receipt)
	server.creations[record.Slug] = receipt
	if err := server.retireCreation(receipt); err != nil {
		t.Fatal(err)
	}
	compact, err := readPreparation(server.preparationPath(record.RequestID, true), server.config.Workspace)
	if err != nil || compact.Snapshot != nil || compact.ReceiptID != record.ReceiptID {
		t.Fatalf("mapping=%#v %v", compact, err)
	}
	if _, err := os.Stat(server.preparationPath(record.RequestID, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("compact mapping remains in CLI scan")
	}
	server.installedTeams = nil
	response := postPreparation(t, server, record.RequestID, record.Snapshot.Input.RawPrompt, "", nil)
	if response.Code != 202 || len(server.creations) != 0 {
		t.Fatalf("retired replay recreated a receipt: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.config.Workspace, "work", record.Slug)); err != nil {
		t.Fatal(err)
	}
	retry := func() *httptest.ResponseRecorder {
		return postCreation(t, server, "/api/session-creations/"+record.RequestID+"/retry",
			fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, record.ReceiptID, compact.Attempt), "application/json")
	}
	var status preparationStatus
	response = retry()
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 || status.State != "ready" || len(server.creations) != 0 {
		t.Fatalf("ready compacted retry=%d %s", response.Code, response.Body.String())
	}
	directory := filepath.Join(server.config.Workspace, "work", record.Slug)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	response = retry()
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 409 || status.State != "gone" || status.ReceiptID != record.ReceiptID || len(server.creations) != 0 {
		t.Fatalf("removed compacted retry=%d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removed destination recreated", err)
	}
}

func TestSessionPreparationPathsRejectAmbiguousIdentifiers(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	id := preparationTestID(1)
	for _, value := range []string{"/api/session-creations/../" + id, "/api/session-creations/%2f" + id, "/api/session-creations/" + id + "/extra", "/creations/" + id + "//", "/creations/%30" + id[1:] + "/"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest("GET", value, nil))
		if response.Code != 404 {
			t.Errorf("path %q status=%d", value, response.Code)
		}
	}
}

func TestSessionPreparationMappingBoundsAndInterruptedMove(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	record := fixturePreparation(t, server, 1, "paused")
	record.State, record.Terminal, record.Phase = "terminal", "ready", "initializing"
	record.Slug = "2026-10-03-recorded-session-work"
	record.Epoch, _ = session.CompletedRemovalHistory(server.config.Workspace, record.Slug, server.config.UserStateRoot)
	record.Snapshot, record.SnapshotDigest = nil, ""
	if err := os.MkdirAll(server.preparationDirectory(false), 0700); err != nil {
		t.Fatal(err)
	}
	path := server.preparationPath(record.RequestID, false)
	encoded, _ := json.Marshal(record)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	server.Close()
	restarted, err := New(server.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := os.Stat(restarted.preparationPath(record.RequestID, true)); err != nil {
		t.Fatal("interrupted move was not finished", err)
	}
	if status, _ := restarted.preparationStatus(record.RequestID); status.State != "gone" || status.CanonicalURL != "" {
		t.Fatal("missing original session was linked")
	}
	record.Error = strings.Repeat("x", maxPreparationMappingBytes)
	encoded, _ = json.Marshal(record)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparation(path, server.config.Workspace); err == nil {
		t.Fatal("oversized compact mapping accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPreparation(path, server.config.Workspace); err == nil {
		t.Fatal("nonprivate record accepted")
	}
}

func TestSessionPreparationPageHasExplicitIdentityAndEscapedRawPrompt(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	record := fixturePreparation(t, server, 91, "paused")
	record.Snapshot.Input.RawPrompt = "  <script>alert('prompt')</script>\nSecond line & last.\n "
	record.Snapshot.Goal = record.Snapshot.Input.RawPrompt
	record.InputDigest = inputPreparationDigest(record.Snapshot.Input)
	record.SnapshotDigest = preparationDigest(record.Snapshot)
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/creations/"+record.RequestID+"/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("preparation page = %d: %s", response.Code, body)
	}
	for _, expected := range []string{
		`data-preparation="` + record.RequestID + `"`, `data-receipt-id="` + record.ReceiptID + `"`,
		`data-accepted-at="` + record.StartedAt + `"`, "&lt;script&gt;", "Second line &amp; last.",
		`/static/preparation.js?v=7`, `/static/creation.js?v=6`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("preparation page omits %q", expected)
		}
	}
	for _, forbidden := range []string{`data-creation=`, `<script>alert`, `<h1>` + record.RequestID + `</h1>`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("preparation page exposes %q", forbidden)
		}
	}
}

func TestSessionPreparationIndexMakesOnlyNewSessionNameOptional(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("index = %d: %s", response.Code, body)
	}
	if strings.Contains(body, `name="name" required`) || !strings.Contains(body, `name="name" maxlength="48" pattern="[A-Za-z0-9][A-Za-z0-9_\-]*"`) {
		t.Fatal("New session must permit an empty name and retain explicit-name limits")
	}
	prompt, options, name := strings.Index(body, `name="goal"`), strings.Index(body, `<summary>Options</summary>`), strings.Index(body, `name="name"`)
	if prompt < 0 || options < prompt || name < options {
		t.Fatal("initial request must precede optional settings")
	}
	if !strings.Contains(body, `id="new-session-recover"`) || !strings.Contains(body, `/static/preparation.js?v=7`) {
		t.Fatal("index is missing durable-request recovery controls")
	}
	if !strings.Contains(body, `href="/" target="_blank" rel="noopener noreferrer"`) || !strings.Contains(body, `Copy saved text`) || !strings.Contains(body, `The original request may still finish.`) {
		t.Fatal("separate request must preserve recovery and open without an opener")
	}
}

func getPreparation(t *testing.T, server *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/session-creations/"+id, nil))
	return w
}

func assertPreparationUnconfirmed(t *testing.T, response *httptest.ResponseRecorder, id string) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 503 ||
		body["requestId"] != id || body["code"] != "preparation_persistence_unconfirmed" || response.Header().Get("Location") != "" {
		t.Fatalf("unconfirmed response=%d %s: %v", response.Code, response.Body.String(), err)
	}
}

func TestSessionPreparationGETSnapshotDoesNotAcceptDelayedIntent(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	id := preparationTestID(1)
	const prompt = "  exact frozen request\nwith a second line  "
	var fault atomic.Bool
	fault.Store(true)
	server.preparationSync = func(directory string) error {
		if fault.Load() && directory == server.preparationDirectory(false) {
			return errors.New("injected admission post-rename sync failure")
		}
		return syncPreparationDirectory(directory)
	}
	var calls atomic.Int32
	entered := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	if response := getPreparation(t, server, id); response.Code != http.StatusNotFound {
		t.Fatalf("initial GET=%d %s", response.Code, response.Body.String())
	}
	// Control the exact GET gap without a callback: confirmation snapshots
	// absence, then a delayed POST publishes an intent before GET projection.
	snapshot, err := server.confirmPreparationStatus(context.Background(), id)
	if err != nil || snapshot.exists {
		t.Fatalf("absent snapshot=%#v %v", snapshot, err)
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- postPreparation(t, server, id, prompt, "", nil) }()
	assertPreparationUnconfirmed(t, <-response, id)
	published, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil || published.State != "accepting" || published.Snapshot.Input.RawPrompt != prompt {
		t.Fatalf("published intent=%#v %v", published, err)
	}
	if status, exists := server.preparationStatusFromSnapshot(id, snapshot); exists || status.ReceiptID != "" {
		t.Fatalf("earlier GET adopted unconfirmed admission: %#v", status)
	}
	assertPreparationUnconfirmed(t, getPreparation(t, server, id), id)
	fault.Store(false)
	// Sync confirmation alone cannot admit an accepting intent or clear the
	// browser's body; admission still needs that identical POST.
	assertPreparationUnconfirmed(t, getPreparation(t, server, id), id)
	page := httptest.NewRecorder()
	server.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/creations/"+id+"/", nil))
	assertPreparationUnconfirmed(t, page, id)
	if calls.Load() != 0 {
		t.Fatal("GET dispatched unaccepted work")
	}
	replay := postPreparation(t, server, id, prompt, "", nil)
	var accepted preparationStatus
	if json.Unmarshal(replay.Body.Bytes(), &accepted) != nil || replay.Code != http.StatusAccepted ||
		accepted.RequestID != id || accepted.ReceiptID != published.ReceiptID || accepted.Attempt != 1 || accepted.InitialRequest != prompt {
		t.Fatalf("identical recovery=%d %s", replay.Code, replay.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("confirmed admission did not launch")
	}
	confirmed := getPreparation(t, server, id)
	var status preparationStatus
	if json.Unmarshal(confirmed.Body.Bytes(), &status) != nil || confirmed.Code != http.StatusOK ||
		status.RequestID != id || status.ReceiptID != published.ReceiptID || status.Attempt != 1 || status.State == "accepting" || calls.Load() != 1 {
		t.Fatalf("confirmed GET=%d %s, calls=%d", confirmed.Code, confirmed.Body.String(), calls.Load())
	}
}

func TestSessionPreparationStatusSnapshotKeepsConfirmedRecordAndWork(t *testing.T) {
	for _, name := range []string{"pending", "dispatched", "stopped"} {
		t.Run(name, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			record := fixturePreparation(t, server, 1, "running")
			if err := server.savePreparation(record); err != nil {
				t.Fatal(err)
			}
			server.operationMu.Lock()
			server.preparationWork[record.RequestID] = preparationWork{attempt: record.Attempt, dispatched: name != "pending", stopped: name == "stopped"}
			server.operationMu.Unlock()
			snapshot, err := server.confirmPreparationStatus(context.Background(), record.RequestID)
			if err != nil || !snapshot.exists {
				t.Fatalf("confirmed snapshot=%#v %v", snapshot, err)
			}
			if err := server.mutatePreparation(context.Background(), record.RequestID, record.Attempt, func(current *sessionPreparation) error {
				current.State, current.Phase, current.Error = "failed", "stopped", "later worker failure"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			server.operationMu.Lock()
			server.preparationWork[record.RequestID] = preparationWork{attempt: record.Attempt + 1}
			server.operationMu.Unlock()
			status, exists := server.preparationStatusFromSnapshot(record.RequestID, snapshot)
			wantState, wantPhase := "paused", "stopped"
			if name == "dispatched" {
				wantState, wantPhase = "running", "naming"
			}
			if !exists || status.State != wantState || status.Phase != wantPhase || status.Error == "later worker failure" ||
				status.ReceiptID != record.ReceiptID || status.Attempt != record.Attempt || status.InitialRequest != record.Snapshot.Input.RawPrompt {
				t.Fatalf("GET reread later record/work: %#v", status)
			}
			if current, _ := server.preparationStatus(record.RequestID); current.State != "failed" {
				t.Fatalf("control did not advance current state: %#v", current)
			}
		})
	}
}

func TestSessionPreparationPublishedAdmissionConfirmationDispatchesOnce(t *testing.T) {
	for _, state := range []string{"accepting", "running"} {
		t.Run(state, func(t *testing.T) {
			server := newTestServer(t)
			release := make(chan struct{})
			defer func() { close(release); server.Close() }()
			id := preparationTestID(1)
			var fault atomic.Bool
			fault.Store(true)
			var calls atomic.Int32
			server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
				calls.Add(1)
				select {
				case <-release:
					return `{"name":"one-two-three"}`, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			server.preparationSync = func(directory string) error {
				if directory == server.preparationDirectory(false) && fault.Load() {
					published, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
					if err == nil && published.State == state {
						return errors.New("injected post-rename sync failure")
					}
				}
				return syncPreparationDirectory(directory)
			}
			assertPreparationUnconfirmed(t, postPreparation(t, server, id, "raw prompt", "", nil), id)
			published, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
			if err != nil || published.State != state {
				t.Fatalf("published=%#v %v", published, err)
			}
			assertPreparationUnconfirmed(t, getPreparation(t, server, id), id)
			assertPreparationUnconfirmed(t, postPreparation(t, server, id, "raw prompt", "", nil), id)
			form := url.Values{"clientRequestId": {id}, "goal": {"raw prompt"}, "name": {""}, "creation_date": {"2026-10-03"}}
			r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(form.Encode()))
			r.Header.Set("Origin", server.config.BaseURL)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			html := httptest.NewRecorder()
			server.Handler().ServeHTTP(html, r)
			assertPreparationUnconfirmed(t, html, id)
			if err := server.reconcilePreparations(server.operationContext); !errors.Is(err, errPreparationPersistenceUnconfirmed) {
				t.Fatalf("collection barrier=%v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("naming started before confirmation")
			}
			fault.Store(false)
			// Collection may confirm bytes, but retains the pending launch for POST.
			if err := server.reconcilePreparations(server.operationContext); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("collector dispatched naming")
			}
			responses := make(chan *httptest.ResponseRecorder, 4)
			for n := 0; n < 4; n++ {
				go func() { responses <- postPreparation(t, server, id, "raw prompt", "", nil) }()
			}
			for n := 0; n < 4; n++ {
				response := <-responses
				var status preparationStatus
				if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 || status.ReceiptID != published.ReceiptID || status.Attempt != 1 {
					t.Fatalf("replay=%d %s", response.Code, response.Body.String())
				}
			}
			deadline := time.Now().Add(time.Second)
			for calls.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if calls.Load() != 1 {
				t.Fatalf("naming calls=%d", calls.Load())
			}
		})
	}
}

func TestSessionPreparationPublishedRetryKeepsAttemptAndLaunch(t *testing.T) {
	server := newTestServer(t)
	release := make(chan struct{})
	defer func() { close(release); server.Close() }()
	record := fixturePreparation(t, server, 1, "failed")
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	var fault atomic.Bool
	fault.Store(true)
	server.preparationSync = func(directory string) error {
		if fault.Load() && directory == server.preparationDirectory(false) {
			return errors.New("injected retry directory sync")
		}
		return syncPreparationDirectory(directory)
	}
	var calls atomic.Int32
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		calls.Add(1)
		select {
		case <-release:
			return `{"name":"one-two-three"}`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	retry := func(attempt int) *httptest.ResponseRecorder {
		return postCreation(t, server, "/api/session-creations/"+record.RequestID+"/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, record.ReceiptID, attempt), "application/json")
	}
	assertPreparationUnconfirmed(t, retry(1), record.RequestID)
	assertPreparationUnconfirmed(t, retry(2), record.RequestID)
	assertPreparationUnconfirmed(t, getPreparation(t, server, record.RequestID), record.RequestID)
	published, err := readPreparation(server.preparationPath(record.RequestID, false), server.config.Workspace)
	if err != nil || published.Attempt != 2 || published.ReceiptID != record.ReceiptID || calls.Load() != 0 {
		t.Fatalf("retry published=%#v %v calls=%d", published, err, calls.Load())
	}
	fault.Store(false)
	var status preparationStatus
	response := getPreparation(t, server, record.RequestID)
	if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 200 || status.State != "paused" || status.Attempt != 2 {
		t.Fatalf("retry offer=%d %s", response.Code, response.Body.String())
	}
	if response := retry(1); response.Code != 409 {
		t.Fatalf("stale retry=%d", response.Code)
	}
	for n := 0; n < 3; n++ {
		response := retry(2)
		if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 || status.Attempt != 2 {
			t.Fatalf("repaired retry=%d %s", response.Code, response.Body.String())
		}
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("naming calls=%d", calls.Load())
	}
}

func TestSessionPreparationStoppedWorkerNeedsExplicitRetryAfterConfirmation(t *testing.T) {
	for _, boundary := range []string{"base", "reservation", "failed"} {
		t.Run(boundary, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			id := preparationTestID(1)
			var fault atomic.Bool
			fault.Store(true)
			var calls atomic.Int32
			server.config.SessionNamer = func(context.Context, string, string) (string, error) {
				calls.Add(1)
				return `{"name":"one-two-three"}`, nil
			}
			var lock *session.RuntimeLock
			if boundary == "failed" {
				var err error
				lock, err = session.LockCreation(server.config.AuthorityDir, "2026-10-03-one-two-three")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			server.preparationSync = func(directory string) error {
				if directory == server.preparationDirectory(false) && fault.Load() {
					published, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
					if err == nil && ((boundary == "base" && published.Base != "") || (boundary == "reservation" && published.Slug != "") || (boundary == "failed" && published.State == "failed")) {
						return errors.New("injected worker directory sync")
					}
				}
				return syncPreparationDirectory(directory)
			}
			if response := postPreparation(t, server, id, "prompt", "", nil); response.Code != 202 {
				t.Fatalf("admission=%d %s", response.Code, response.Body.String())
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				server.operationMu.Lock()
				stopped := server.preparationWork[id].stopped
				server.operationMu.Unlock()
				if stopped {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not stop")
				}
				time.Sleep(time.Millisecond)
			}
			assertPreparationUnconfirmed(t, getPreparation(t, server, id), id)
			published, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
			if err != nil || published.Base != "one-two-three" || calls.Load() != 1 {
				t.Fatalf("stopped=%#v %v", published, err)
			}
			fault.Store(false)
			if lock != nil {
				lock.Close()
			}
			var status preparationStatus
			response := getPreparation(t, server, id)
			if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 200 || (status.State != "paused" && status.State != "failed") {
				t.Fatalf("stopped offer=%d %s", response.Code, response.Body.String())
			}
			if response := postPreparation(t, server, id, "prompt", "", nil); response.Code != 202 {
				t.Fatal(response.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatal("identical POST redispatched a stopped worker")
			}
			response = postCreation(t, server, "/api/session-creations/"+id+"/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, published.ReceiptID), "application/json")
			if response.Code != 202 {
				t.Fatal(response.Body.String())
			}
			result := awaitPreparation(t, server, id)
			if result.Attempt != 2 || result.ReceiptID != published.ReceiptID || result.Base != published.Base || result.Slug == "" || (published.Slug != "" && result.Slug != published.Slug) || calls.Load() != 1 {
				t.Fatalf("explicit retry=%#v calls=%d", result, calls.Load())
			}
		})
	}
}

func TestSessionPreparationMappingConfirmationPrecedesRetirement(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(fmt.Sprint(moved), func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			record := fixturePreparation(t, server, 1, "handed_off")
			record.Base, record.Slug, record.Phase = "one-two-three", "2026-10-03-one-two-three", "initializing"
			record.Epoch, _ = session.CompletedRemovalHistory(server.config.Workspace, record.Slug, server.config.UserStateRoot)
			request := preparationCreationRequest(record)
			record.Handoff = &request
			if err := server.savePreparation(record); err != nil {
				t.Fatal(err)
			}
			receipt := creationReceipt{Schema: 1, Workspace: record.Workspace, Request: request, ReceiptID: record.ReceiptID, DeletionHistorySHA256: record.Epoch, Attempt: 1, State: "ready", Validated: true, Goal: record.Snapshot.Goal, StartedAt: record.StartedAt, UpdatedAt: record.UpdatedAt}
			if err := server.saveCreation(receipt); err != nil {
				t.Fatal(err)
			}
			writeCreationProof(t, server, receipt)
			server.creations[record.Slug] = receipt
			var fault atomic.Bool
			fault.Store(true)
			server.preparationSync = func(directory string) error {
				if fault.Load() && directory == server.preparationDirectory(moved) {
					published, err := readPreparation(server.preparationPath(record.RequestID, moved), server.config.Workspace)
					if err == nil && published.State == "terminal" {
						return errors.New("injected mapping directory sync")
					}
				}
				return syncPreparationDirectory(directory)
			}
			for n := 0; n < 2; n++ {
				if err := server.retireCreation(receipt); !errors.Is(err, errPreparationPersistenceUnconfirmed) {
					t.Fatalf("retire=%v", err)
				}
				if _, err := os.Stat(server.creationPath(record.Slug)); err != nil {
					t.Fatalf("receipt released=%v", err)
				}
			}
			fault.Store(false)
			if err := server.retireCreation(receipt); err != nil {
				t.Fatal(err)
			}
			mapping, err := readPreparation(server.preparationPath(record.RequestID, true), server.config.Workspace)
			if err != nil || mapping.ReceiptID != record.ReceiptID || mapping.Epoch != record.Epoch {
				t.Fatalf("mapping=%#v %v", mapping, err)
			}
		})
	}
}

func TestSessionPreparationPendingConfirmationRejectsSupersededGeneration(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	id := preparationTestID(1)
	var fault atomic.Bool
	fault.Store(true)
	var syncs atomic.Int32
	server.preparationSync = func(directory string) error {
		syncs.Add(1)
		if fault.Load() && directory == server.preparationDirectory(false) {
			return errors.New("injected directory sync")
		}
		return syncPreparationDirectory(directory)
	}
	var calls atomic.Int32
	server.config.SessionNamer = func(context.Context, string, string) (string, error) {
		calls.Add(1)
		return `{"name":"one-two-three"}`, nil
	}
	assertPreparationUnconfirmed(t, postPreparation(t, server, id, "prompt", "", nil), id)
	fault.Store(false)
	if err := os.Remove(server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), server.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	before := syncs.Load()
	for _, response := range []*httptest.ResponseRecorder{postPreparation(t, server, id, "prompt", "", nil), getPreparation(t, server, id)} {
		if response.Code != 503 {
			t.Fatalf("superseded=%d %s", response.Code, response.Body.String())
		}
	}
	if calls.Load() != 0 || syncs.Load() != before {
		t.Fatal("superseded generation confirmed or dispatched")
	}
}

func TestSessionPreparationAcceptingUploadConfirmationRetainsBeforeCollection(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.uploadStore.MinFreeBytes = 0
	ctx := context.Background()
	scope, err := server.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID}
	bytes := "selected bytes survive a published intent"
	file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(999), Name: "retained.txt", Size: int64(len(bytes))})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(bytes))
	if _, err := backend.Append(ctx, file.ID, 0, hex.EncodeToString(digest[:]), strings.NewReader(bytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	id := preparationTestID(1)
	var fault atomic.Bool
	fault.Store(true)
	server.preparationSync = func(directory string) error {
		if fault.Load() && directory == server.preparationDirectory(false) {
			return errors.New("injected accepting post-rename sync")
		}
		return syncPreparationDirectory(directory)
	}
	var calls atomic.Int32
	entered := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, _ string, _ string) (string, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	extra := url.Values{"uploadScope": {scope.ID}, "attachmentIds": {file.ID}}
	assertPreparationUnconfirmed(t, postPreparation(t, server, id, " raw prompt ", "", extra), id)
	published, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil || published.State != "accepting" || published.Snapshot.Goal != "" {
		t.Fatalf("intent=%#v %v", published, err)
	}
	server.uploadStore.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if err := server.collectUploads(ctx); !errors.Is(err, errPreparationPersistenceUnconfirmed) {
		t.Fatalf("uncertain collection=%v", err)
	}
	fault.Store(false)
	for n := 0; n < 2; n++ {
		if err := server.collectUploads(ctx); err != nil {
			t.Fatal(err)
		}
	}
	content, err := backend.Open(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(content.File)
	content.File.Close()
	if err != nil || string(actual) != bytes {
		t.Fatalf("selected bytes=%q %v", actual, err)
	}
	if err := backend.Delete(ctx, file.ID, true); err == nil {
		t.Fatal("pending files were released")
	}
	server.operationMu.Lock()
	retained := server.preparations[id]
	work := server.preparationWork[id]
	server.operationMu.Unlock()
	if retained.ReceiptID != published.ReceiptID || retained.Attempt != 1 || retained.Snapshot.Input.UploadScope != scope.ID || !strings.Contains(retained.Snapshot.Goal, "retained.txt") || work.dispatched || calls.Load() != 0 {
		t.Fatalf("retained=%#v work=%#v calls=%d", retained, work, calls.Load())
	}
	if response := postPreparation(t, server, preparationTestID(2), " raw prompt ", "", extra); response.Code != 409 {
		t.Fatalf("competing ownership=%d", response.Code)
	}
	for n := 0; n < 2; n++ {
		response := postPreparation(t, server, id, " raw prompt ", "", extra)
		var status preparationStatus
		if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 || status.ReceiptID != published.ReceiptID || status.Attempt != 1 {
			t.Fatalf("repair=%d %s", response.Code, response.Body.String())
		}
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("pending worker was not launched")
	}
	if calls.Load() != 1 {
		t.Fatalf("naming calls=%d", calls.Load())
	}
}

func TestPreparationBackendWireIdentity(t *testing.T) {
	base := url.Values{"clientRequestId": {preparationTestID(1)}, "goal": {"literal goal"}, "creation_date": {"2026-10-03"}, "name": {"solo"}}
	parse := func(fields url.Values) (preparationInput, error) {
		r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(fields.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		_, input, err := preparationForm(r)
		return input, err
	}
	legacy, err := parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"", "codex"} {
		fields := url.Values{}
		for key, values := range base {
			fields[key] = append([]string(nil), values...)
		}
		fields.Set("backend", backend)
		input, err := parse(fields)
		if err != nil || inputPreparationDigest(input) != inputPreparationDigest(legacy) {
			t.Fatalf("explicit Codex changed old input identity: %v", err)
		}
	}
	fields := url.Values{}
	for key, values := range base {
		fields[key] = append([]string(nil), values...)
	}
	fields.Set("backend", "pi")
	fields.Set("piProviderDigest", strings.Repeat("a", 64))
	pi, err := parse(fields)
	if err != nil {
		t.Fatalf("Pi arm rejected: %v", err)
	}
	if inputPreparationDigest(pi) == inputPreparationDigest(legacy) {
		t.Fatal("Pi and Codex input identities collapsed")
	}
	for _, invalid := range []url.Values{
		{"backend": {"unknown"}}, {"backend": {"pi", "codex"}}, {"piProviderDigest": {strings.Repeat("a", 64)}},
	} {
		fields := url.Values{}
		for key, values := range base {
			fields[key] = append([]string(nil), values...)
		}
		for key, values := range invalid {
			fields[key] = values
		}
		if _, err := parse(fields); err == nil {
			t.Fatalf("contradictory backend fields accepted: %v", invalid)
		}
	}
}

// This is controlled pre-root store data, not a native creation proof.
func piPreparationStoreFixture(t *testing.T) (*Server, sessionPreparation) {
	t.Helper()
	s := newTestServer(t)
	s.config.SessionNamer = func(context.Context, string, string) (string, error) {
		return "", errors.New("fixture stops before reservation")
	}
	id := preparationTestID(1)
	if response := postPreparation(t, s, id, "literal goal", "", nil); response.Code != http.StatusAccepted {
		t.Fatal(response.Body.String())
	}
	record := awaitPreparation(t, s, id)
	configuration := piProviderPackageFixture(t)
	preset := piCreationSoloPreset()
	preset.LeadModel = "saved-lead"
	input := record.Snapshot.Input
	input.Backend, input.CustomName = "pi", "solo"
	input.Team, input.Catalog = preparationValue{true, preset.ID}, preparationValue{true, preset.CatalogDigest}
	input.Model, input.Effort = preparationValue{true, preset.LeadModel}, preparationValue{true, preset.LeadEffort}
	frozen := preparationPi{CatalogDigest: configuration.Catalog.Digest, Default: copyPiSelection(configuration.Default)}
	input.PiProviderDigest = piPreparationProviderDigest(frozen)
	record.Schema, record.InputVersion, record.NamingOutcome = 2, 2, ""
	record.Snapshot = &preparationSnapshot{Input: input, Preset: &preset, Pi: &frozen, Goal: "literal goal"}
	record.InputDigest, record.SnapshotDigest = versionedPreparationDigest(input, record.InputVersion), preparationDigest(record.Snapshot)
	s.operationMu.Lock()
	err := s.savePreparation(record)
	s.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return s, record
}

func TestPiPreparationInventoryBothDirectoriesAndCompactIdentity(t *testing.T) {
	s, record := piPreparationStoreFixture(t)
	defer s.Close()
	path := s.preparationPath(record.RequestID, false)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspect := func() {
		t.Helper()
		refs, err := InspectRetainedPiPreparations(context.Background(), s.config.UserStateRoot, []string{s.config.Workspace})
		if err != nil || len(refs) != 1 || refs[0] != (PiPreparationReference{Workspace: s.config.Workspace, RequestID: record.RequestID, Schema: record.Schema}) {
			t.Fatalf("inventory=%v %v", refs, err)
		}
	}
	inspect()
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("read-only inventory rewrote a request")
	}
	if _, err := InspectRetainedPiPreparations(context.Background(), s.config.UserStateRoot, nil); err == nil {
		t.Fatal("orphan Pi request omitted")
	}
	record.State, record.Phase, record.Terminal = "terminal", "initializing", "ready"
	record.Slug, record.Epoch = "2026-10-03-solo", strings.Repeat("a", 64)
	record.Snapshot, record.Handoff, record.SnapshotDigest, record.Base, record.NamingOutcome = nil, nil, "", "", ""
	s.operationMu.Lock()
	err = s.savePreparation(record)
	s.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	inspect()
	// A crash after compaction but before the directory move is still Pi.
	if err := os.Rename(s.preparationPath(record.RequestID, true), path); err != nil {
		t.Fatal(err)
	}
	inspect()
	status, ok := s.preparationStatus(record.RequestID)
	if !ok || status.Backend != "pi" {
		t.Fatal("compaction lost backend identity")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRetainedPiPreparations(context.Background(), s.config.UserStateRoot, []string{s.config.Workspace}); err == nil {
		t.Fatal("public authoritative file accepted")
	}
}

func TestPiPreparationVersionedCandidateSwitchAndRollback(t *testing.T) {
	s, legacy := piPreparationStoreFixture(t)
	defer s.Close()
	for _, name := range []string{"repos", "work", "worktrees"} {
		if err := os.MkdirAll(filepath.Join(s.config.Workspace, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Retain the old body byte-for-byte and add a separate new-version request
	// only after the candidate-owned install has completed.
	data, _ := json.Marshal(legacy)
	var next sessionPreparation
	if err := json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	next.Schema, next.InputVersion, next.RequestID = 3, 3, preparationTestID(2)
	next.InputDigest = versionedPreparationDigest(next.Snapshot.Input, 3)
	if err := validatePreparation(next, s.config.Workspace); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	newSource := filepath.Join(directory, "new.json")
	data, _ = json.Marshal(next)
	if err := os.WriteFile(newSource, data, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(directory, "workspace-portal")
	moduleMode := "readonly"
	if info, err := os.Stat(filepath.Join(source, "portal", "vendor")); err == nil && info.IsDir() {
		moduleMode = "vendor" // The packaged offline check owns its pinned vendor tree.
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-mod="+moduleMode, "-o", helper, "./cmd/workspace-portal")
	build.Dir = filepath.Join(source, "portal")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("owning scanner build: %v\n%s", err, output)
	}
	input := map[string]string{"workspace": s.config.Workspace, "state": s.config.UserStateRoot,
		"profile": filepath.Join(directory, "profile"), "legacy": s.preparationPath(legacy.RequestID, false),
		"newSource": newSource, "newDestination": s.preparationPath(next.RequestID, false)}
	path := filepath.Join(directory, "transition.json")
	data, _ = json.Marshal(input)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("ruby", filepath.Join(source, "test/support/pi_team_preflight_driver.rb"))
	command.Env = append(os.Environ(), "P2_TEAM_SOURCE_ROOT="+source, "P2_TEAM_PORTAL_HELPER="+helper, "P2_PI_PREPARATION_INPUT="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("candidate-owned preparation switch: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

func TestPiPreparationCodecRefusesMixedSnapshots(t *testing.T) {
	s, original := piPreparationStoreFixture(t)
	defer s.Close()
	data, _ := json.Marshal(original)
	for name, change := range map[string]func(map[string]any){
		"schema1": func(v map[string]any) { v["schema"] = 1 },
		"input1":  func(v map[string]any) { v["inputVersion"] = 1 },
		"backend": func(v map[string]any) { v["snapshot"].(map[string]any)["input"].(map[string]any)["backend"] = "codex" },
		"default": func(v map[string]any) { v["snapshot"].(map[string]any)["pi"].(map[string]any)["default"] = nil },
		"unknown": func(v map[string]any) { v["unexpected"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			change(raw)
			bad, _ := json.Marshal(raw)
			path := filepath.Join(t.TempDir(), "bad.json")
			if err := os.WriteFile(path, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readPreparation(path, s.config.Workspace); err == nil {
				t.Fatal("mixed record accepted")
			}
		})
	}
}

func TestPiPreparationPublicRefusalDoesNotPublishOrReserve(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	preset := piCreationSoloPreset()
	s.installedTeams = piCreationTeamCatalogFixture(t, preset)
	configuration := piProviderPackageFixture(t)
	services, err := newPiSelectionServices(Config{Workspace: s.config.Workspace, UserStateRoot: s.config.UserStateRoot,
		PiAuthDirectory: filepath.Join(t.TempDir(), "auth"), PiHostStateRoot: filepath.Join(t.TempDir(), "host"), PiProviderConfig: &configuration})
	if err != nil {
		t.Fatal(err)
	}
	f := &piCreationRuntimeFixture{}
	services.Runtime = f
	services.StartRequest = func(session.PiCreationIntent, uint64) (piruntime.StartRequest, error) {
		t.Error("refused preparation reached StartRequest")
		return piruntime.StartRequest{}, errors.New("unexpected start")
	}
	selected := 0
	services.Selection = func(context.Context, agentbackend.SessionRef, []session.PiReservedConversation, teamruntime.Preset) (piproviders.Policy, error) {
		selected++
		return piproviders.Policy{}, errors.New("unexpected selection")
	}
	s.config.PiCreation, s.config.PiProviderConfig = services, &configuration
	for name, fields := range map[string]url.Values{
		"unavailable package": {"backend": {"pi"}, "team": {"solo"}, "catalogDigest": {preset.CatalogDigest}, "model": {"model"}, "effort": {"high"}, "piProviderDigest": {strings.Repeat("a", 64)}},
		"duplicate backend":   {"backend": {"pi", "codex"}},
		"unknown backend":     {"backend": {"other"}},
		"mixed codex token":   {"backend": {"codex"}, "piProviderDigest": {strings.Repeat("a", 64)}},
	} {
		t.Run(name, func(t *testing.T) {
			response := postPreparation(t, s, preparationTestID(1), "literal goal", "solo", fields)
			if response.Code < 400 {
				t.Fatalf("unexpected acceptance: %d %s", response.Code, response.Body.String())
			}
			if len(s.preparations) != 0 || len(s.creations) != 0 {
				t.Fatal("refused input published durable identity")
			}
			if selected != 0 || f.ensures != 0 || f.creates != 0 || f.sends != 0 {
				t.Fatal("refused public input crossed a native effect boundary")
			}
			if _, err := os.Stat(s.preparationDirectory(false)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused input created preparation directory", err)
			}
		})
	}
}

func TestPiPreparationReplayDoesNotConsultCurrentComposition(t *testing.T) {
	s, record := piPreparationStoreFixture(t)
	defer s.Close()
	i := record.Snapshot.Input
	extra := url.Values{"backend": {"pi"}, "piProviderDigest": {i.PiProviderDigest}, "team": {i.Team.Value}, "catalogDigest": {i.Catalog.Value}, "model": {i.Model.Value}, "effort": {i.Effort.Value}}
	// No runtime/composition exists in this controlled failed store fixture.
	response := postPreparation(t, s, record.RequestID, i.RawPrompt, i.CustomName, extra)
	if response.Code != http.StatusAccepted {
		t.Fatalf("original lookup consulted current availability: %d %s", response.Code, response.Body.String())
	}
	var status preparationStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Backend != "pi" || status.ReceiptID != record.ReceiptID {
		t.Fatal(status, err)
	}
	extra.Set("piProviderDigest", strings.Repeat("f", 64))
	if changed := postPreparation(t, s, record.RequestID, i.RawPrompt, i.CustomName, extra); changed.Code != http.StatusConflict {
		t.Fatal("changed original input accepted")
	}
	for _, team := range []bool{false, true} {
		t.Run(fmt.Sprintf("handed-off-team=%t", team), func(t *testing.T) {
			server, accepted, native := piPreparationReceiptFixture(t, team)
			input := accepted.Snapshot.Input
			fields := url.Values{"backend": {"pi"}, "piProviderDigest": {input.PiProviderDigest}, "team": {input.Team.Value}, "catalogDigest": {input.Catalog.Value}, "model": {input.Model.Value}, "effort": {input.Effort.Value}}
			services, installed := server.config.PiCreation, server.installedTeams
			server.config.PiCreation, server.installedTeams = nil, nil
			response := postPreparation(t, server, accepted.RequestID, input.RawPrompt, input.CustomName, fields)
			var status preparationStatus
			if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 || status.State != "failed" || status.Slug != accepted.Slug || status.ReceiptID != accepted.ReceiptID || status.CanonicalURL == "" {
				t.Fatal("accepted receipt consulted current composition", response.Code, response.Body.String())
			}
			server.config.PiCreation, server.installedTeams = services, installed
			server.Close()
			// The existing fixture installs its fake package context after New.
			// Reload durable state through that same constructor boundary.
			config := server.config
			config.PackageRoot = ""
			restarted, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restarted.config.PackageRoot = server.config.PackageRoot
			restarted.installedTeams = installed
			status, ok := restarted.preparationStatus(accepted.RequestID)
			if !ok || status.State != "failed" || status.Slug != accepted.Slug || status.ReceiptID != accepted.ReceiptID {
				t.Fatal("restart lost original handoff", status)
			}
			body := fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, accepted.ReceiptID, status.Attempt)
			if response := postCreation(t, restarted, "/api/session-creations/"+accepted.RequestID+"/retry", body, "application/json"); response.Code != 202 {
				t.Fatal("exact receipt recovery", response.Code, response.Body.String())
			}
			restarted.operationWG.Wait()
			status, ok = restarted.preparationStatus(accepted.RequestID)
			if !ok || status.State != "ready" || status.ReceiptID != accepted.ReceiptID || native.creates != 1 || native.sends != 1 {
				t.Fatal("receipt recovery repeated original effects", status, native.creates, native.sends)
			}
			compact, err := readPreparation(restarted.preparationPath(accepted.RequestID, true), restarted.config.Workspace)
			if err != nil || compact.State != "terminal" || compact.Snapshot != nil || compact.ReceiptID != accepted.ReceiptID {
				t.Fatal("ready receipt did not compact exact preparation", compact, err)
			}
			restarted.operationMu.Lock()
			err = restarted.retireCreation(restarted.creations[accepted.Slug])
			restarted.operationMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			response = postPreparation(t, restarted, accepted.RequestID, input.RawPrompt, input.CustomName, fields)
			if json.Unmarshal(response.Body.Bytes(), &status) != nil || response.Code != 202 || status.State != "ready" || native.creates != 1 || native.sends != 1 {
				t.Fatal("retired receipt replay repeated original effects", response.Code, response.Body.String())
			}
		})
	}
}

// Seed an accepted frozen request through the existing record fixture pattern,
// then use real handoff/HTTP owners and the existing fake runtime. Stop after
// the original Accepted input so restart must recover that exact receipt.
func piPreparationReceiptFixture(t *testing.T, team bool) (*Server, sessionPreparation, *piUsabilityRuntime) {
	t.Helper()
	s, receipt, native := newPiCreationFixtureForSlug(t, "2026-10-03-receipt-team")
	t.Cleanup(s.Close)
	preset := piCreationSoloPreset()
	preset.LeadModel = "saved-lead"
	if team {
		preset.ID = "delegated"
		preset.Members = []teamruntime.MemberSpec{{Model: "saved-member", Effort: "high", Access: "workspace_write"}}
	}
	s.installedTeams = piCreationTeamCatalogFixture(t, preset)
	preset, err := teamruntime.FindCatalogPreset(*s.installedTeams.Catalog, preset.ID)
	if err != nil {
		t.Fatal(err)
	}
	configuration := piProviderPackageFixture(t)
	services, err := newPiSelectionServices(Config{Workspace: s.config.Workspace, UserStateRoot: s.config.UserStateRoot, PiAuthDirectory: filepath.Join(t.TempDir(), "auth"), PiHostStateRoot: filepath.Join(t.TempDir(), "host"), PiProviderConfig: &configuration})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &piUsabilityRuntime{piCreationRuntimeFixture: native, receipts: map[string]piruntime.SubmissionStatus{}}
	services.Runtime, services.StartRequest = runtime, s.config.PiCreation.StartRequest
	s.config.PiCreation = services // Keep New's restart bound to this fake port.
	s.piWorkflows = newPiWorkflowComposition(s)
	s.piCreationCheckpoint = func(point string) error {
		if point == "initial_input" {
			return errors.New("fixture stops after Accepted input")
		}
		return nil
	}
	frozen := services.providers.preparationProvider()
	record := fixturePreparation(t, s, 800, "running")
	record.Schema, record.InputVersion = 3, 3
	record.ReceiptID, record.Epoch = receipt.ReceiptID, receipt.DeletionHistorySHA256
	record.Base, record.Slug, record.NamingOutcome = "receipt-team", receipt.Request.Slug, "custom"
	input := record.Snapshot.Input
	input.Backend, input.CustomName, input.PiProviderDigest = "pi", record.Base, piPreparationProviderDigest(frozen)
	input.Team, input.Catalog = preparationValue{true, preset.ID}, preparationValue{true, preset.CatalogDigest}
	input.Model, input.Effort = preparationValue{true, preset.LeadModel}, preparationValue{true, preset.LeadEffort}
	record.Snapshot = &preparationSnapshot{Input: input, Goal: input.RawPrompt, Preset: &preset, Pi: &frozen}
	record.InputDigest, record.SnapshotDigest = versionedPreparationDigest(input, 3), preparationDigest(record.Snapshot)
	request := preparationCreationRequest(record)
	record.Handoff = &request
	receipt.Request, receipt.DirectTeam, receipt.Goal = request, &preset, record.Snapshot.Goal
	receipt.Model, receipt.Effort = preset.LeadModel, preset.LeadEffort
	receipt.Pi.Conversations = nil
	for index, address := range preset.Roles {
		receipt.Pi.Conversations = append(receipt.Pi.Conversations, session.PiReservedConversation{Address: address, Incarnation: strings.Repeat(string(rune('b'+index)), 64)})
	}
	ref := agentbackend.SessionRef{Workspace: receipt.Workspace, Slug: record.Slug, Incarnation: receipt.Pi.SessionIncarnation}
	policy, err := services.Selection(context.Background(), ref, receipt.Pi.Conversations, preset)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Pi.Selection, err = session.FreezePiCreationSelection(services.Catalog, policy, receipt.Pi.Conversations)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Pi.Provider = receipt.Pi.Selection.Members[0].Settings.Provider
	if err := s.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	s.creations[record.Slug] = receipt
	if err := s.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	if err := s.handoffPreparationLocked(&record, record.Slug); err != nil {
		t.Fatal(err)
	}
	if err := s.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	s.operationWG.Add(1)
	s.runCreation(receipt)
	if record.State != "handed_off" || native.creates != 1 || native.sends != 1 || native.submission == nil || native.submission.Phase != "Accepted" {
		t.Fatal("fixture did not hand off original Accepted input", record, native.creates, native.sends)
	}
	return s, record, runtime
}

func TestPiPreparationReceiptRequiresExactFrozenTeam(t *testing.T) {
	s, record, _ := piPreparationReceiptFixture(t, true)
	receipt := s.creations[record.Slug]
	if len(record.Snapshot.Preset.Members) != 2 || record.Snapshot.Preset.Members[0].Model == record.Snapshot.Preset.LeadModel || !preparationReceiptMatches(record, receipt) {
		t.Fatal("full frozen team did not match its original receipt")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*creationReceipt){
		"missing member": func(r *creationReceipt) { r.Pi.Selection.Members = r.Pi.Selection.Members[:2] },
		"extra member": func(r *creationReceipt) {
			r.Pi.Selection.Members = append(r.Pi.Selection.Members, r.Pi.Selection.Members[1])
		},
		"duplicate member": func(r *creationReceipt) { r.Pi.Selection.Members[2] = r.Pi.Selection.Members[1] },
		"missing override": func(r *creationReceipt) { r.Pi.Selection.Policy.Overrides = r.Pi.Selection.Policy.Overrides[:2] },
		"extra override": func(r *creationReceipt) {
			r.Pi.Selection.Policy.Overrides = append(r.Pi.Selection.Policy.Overrides, r.Pi.Selection.Policy.Overrides[1])
		},
		"duplicate override":  func(r *creationReceipt) { r.Pi.Selection.Policy.Overrides[2] = r.Pi.Selection.Policy.Overrides[1] },
		"sibling incarnation": func(r *creationReceipt) { r.Pi.Conversations[1].Incarnation = strings.Repeat("f", 64) },
		"sibling model":       func(r *creationReceipt) { r.Pi.Selection.Members[1].Settings.Model = "saved-lead" },
		"sibling effort":      func(r *creationReceipt) { r.Pi.Selection.Members[1].Settings.Thinking = "off" },
		"override incarnation": func(r *creationReceipt) {
			r.Pi.Selection.Policy.Overrides[1].Member.Incarnation = strings.Repeat("f", 64)
		},
		"override model": func(r *creationReceipt) {
			r.Pi.Selection.Policy.Overrides[1].Value.Selection.Chain[0].ModelID = "saved-lead"
		},
		"override effort": func(r *creationReceipt) { r.Pi.Selection.Policy.Overrides[1].Value.Selection.Chain[0].Thinking = "off" },
		"account": func(r *creationReceipt) {
			r.Pi.Selection.Policy.Overrides[1].Value.Selection.Chain[0].AccountRef = "other-account"
		},
		"fallback": func(r *creationReceipt) {
			r.Pi.Selection.Policy.Overrides[1].Value.Selection.Chain[1].ModelID = "saved-lead"
		},
		"categories": func(r *creationReceipt) {
			r.Pi.Selection.Policy.Overrides[1].Value.Selection.FallbackOn = []piproviders.FailureCategory{}
		},
		"budget":            func(r *creationReceipt) { r.Pi.Selection.Policy.Overrides[1].Value.Selection.Budget.MaxHops = 0 },
		"default":           func(r *creationReceipt) { r.Pi.Selection.Policy.Default.Selection.Chain[0].Thinking = "off" },
		"default revision":  func(r *creationReceipt) { r.Pi.Selection.Policy.Default.Revision = 2 },
		"override revision": func(r *creationReceipt) { r.Pi.Selection.Policy.Overrides[1].Value.Revision = 2 },
		"policy revision":   func(r *creationReceipt) { r.Pi.Selection.Policy.Revision = 2 },
		"catalog":           func(r *creationReceipt) { r.Pi.Selection.CatalogDigest = strings.Repeat("f", 64) },
		"workspace":         func(r *creationReceipt) { r.Workspace += "-other" },
		"receipt":           func(r *creationReceipt) { r.ReceiptID = strings.Repeat("f", 64) },
		"epoch":             func(r *creationReceipt) { r.DeletionHistorySHA256 = strings.Repeat("f", 64) },
		"request":           func(r *creationReceipt) { r.Request.Goal += " changed" },
		"preset":            func(r *creationReceipt) { r.DirectTeam.Members[1].Instructions += " changed" },
	} {
		t.Run(name, func(t *testing.T) {
			var changed creationReceipt
			if err := json.Unmarshal(encoded, &changed); err != nil {
				t.Fatal(err)
			}
			change(&changed)
			if preparationReceiptMatches(record, changed) {
				t.Fatal("changed frozen receipt matched original preparation")
			}
		})
	}
}

func TestPiPreparationFrozenPresetRecheckPreservesOriginalRole(t *testing.T) {
	s := newTestServer(t)
	defer s.Close()
	preset := piCreationSoloPreset()
	s.installedTeams = piCreationTeamCatalogFixture(t, preset)
	input := preparationInput{Backend: "pi", Team: preparationValue{true, "solo"}, Catalog: preparationValue{true, preset.CatalogDigest}, Model: preparationValue{true, preset.LeadModel}, Effort: preparationValue{true, preset.LeadEffort}}
	frozen, err := s.freezePreparationPreset(input)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &preparationSnapshot{Input: input, Preset: frozen}
	before, _ := json.Marshal(snapshot)
	if err := s.validatePiPreparationPreset(snapshot); err != nil {
		t.Fatal(err)
	}
	s.installedTeams.Catalog.CatalogDigest = strings.Repeat("f", 64)
	if err := s.validatePiPreparationPreset(snapshot); !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal("changed installed catalog replaced original preset", err)
	}
	s.installedTeams.Catalog.CatalogDigest = preset.CatalogDigest
	team := s.installedTeams.Catalog.Teams["solo"]
	role := team.Roles["team_lead"]
	role.Instructions = "Changed literal role"
	team.Roles["team_lead"] = role
	s.installedTeams.Catalog.Teams["solo"] = team
	if err := s.validatePiPreparationPreset(snapshot); !errors.Is(err, piruntime.ErrHeld) {
		t.Fatal("changed saved-role source accepted", err)
	}
	after, _ := json.Marshal(snapshot)
	if !bytes.Equal(before, after) {
		t.Fatal("recheck rebuilt original immutable intent")
	}
}

// This fixture keeps real preparation, upload, admission and HTTP owners; only
// native execution is replaced. The worker's actual image projection has its
// separate pinned SDK fixture in session.test.mjs.
type piUsabilityRuntime struct {
	*piCreationRuntimeFixture
	receipts    map[string]piruntime.SubmissionStatus
	entries     []backend.TranscriptEntry
	busy        bool
	unknownOnce bool
}

func (f *piUsabilityRuntime) Bind(_ context.Context, h agentbackend.ExecutionHandle) (backend.Operations, error) {
	if h.Conversation != f.state.Conversation || h.Generation != f.state.Generation {
		return backend.Operations{}, piruntime.ErrScope
	}
	return backend.Operations{Verify: f, Read: f, Page: f, Send: f}, nil
}
func (f *piUsabilityRuntime) Verify(_ context.Context, id backend.Identity) error {
	if id != piruntime.BackendIdentity(agentbackend.ExecutionHandle{Conversation: f.state.Conversation, Generation: f.state.Generation}) {
		return piruntime.ErrScope
	}
	return nil
}
func (f *piUsabilityRuntime) Send(ctx context.Context, id backend.Identity, input backend.SendRequest) (backend.SendResult, error) {
	if old, ok := f.receipts[input.ClientUserMessageID]; ok {
		return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: old.Ref.OperationID, TurnID: old.Witness.EntryID}}, nil
	}
	if _, err := f.piCreationRuntimeFixture.Send(ctx, id, input); err != nil {
		return backend.SendResult{}, err
	}
	record := *f.submission
	record.Witness.EntryID = "user-" + input.ClientUserMessageID
	f.receipts[input.ClientUserMessageID] = record
	f.entries = append(f.entries, backend.TranscriptEntry{Kind: "userMessage", ItemID: record.Witness.EntryID, TurnID: record.Witness.EntryID, Text: input.Message, ClientUserMessageID: input.ClientUserMessageID, ClientUserMessageDigest: piruntime.MessageDigest(input.Message)}, backend.TranscriptEntry{Kind: "agentMessage", ItemID: "reply-" + input.ClientUserMessageID, TurnID: record.Witness.EntryID, Text: "Done.\n\n- inspected input"})
	if f.unknownOnce {
		f.unknownOnce = false
		return backend.SendResult{Outcome: backend.Unknown}, nil
	}
	return backend.SendResult{Outcome: backend.Accepted, Receipt: backend.SendReceipt{ClientUserMessageID: input.ClientUserMessageID, TurnID: record.Witness.EntryID}}, nil
}
func (f *piUsabilityRuntime) Lookup(_ context.Context, ref piruntime.SubmissionRef) (piruntime.SubmissionStatus, error) {
	r, ok := f.receipts[ref.OperationID]
	if !ok {
		return r, os.ErrNotExist
	}
	if r.Ref != ref {
		return r, piruntime.ErrConflict
	}
	return r, nil
}
func (f *piUsabilityRuntime) VerifyCreation(ctx context.Context, h agentbackend.ExecutionHandle, id, history string, bootstrap *piruntime.SubmissionRef) (piruntime.CreationProof, error) {
	proof, err := f.piCreationRuntimeFixture.VerifyCreation(ctx, h, id, history, nil)
	if bootstrap != nil {
		if r, ok := f.receipts[bootstrap.OperationID]; ok && r.Ref == *bootstrap {
			proof.Bootstrap = &r
			proof.Persisted = true
		}
	}
	return proof, err
}
func (f *piUsabilityRuntime) Read(ctx context.Context, id backend.Identity) (backend.Transcript, error) {
	if err := f.Verify(ctx, id); err != nil {
		return backend.Transcript{}, err
	}
	state := "idle"
	if f.busy {
		state = "active"
	}
	return backend.Transcript{ThreadID: f.state.Conversation.NativeID, Status: state, Model: f.state.Settings.Model, ReasoningEffort: f.state.Settings.Thinking, Entries: append([]backend.TranscriptEntry(nil), f.entries...)}, nil
}
func (f *piUsabilityRuntime) ReadPage(ctx context.Context, id backend.Identity, cursor backend.HistoryCursor) (backend.TranscriptPage, error) {
	t, err := f.Read(ctx, id)
	return backend.TranscriptPage{Transcript: t}, err
}

func TestPiUsabilityPreparationGenericFilesNamingAndScopedReload(t *testing.T) {
	root := os.Getenv("PI_USABILITY_PACKAGE")
	if root == "" {
		t.Skip("explicit immutable metadata fixture required")
	}
	s, _, native := newPiCreationFixture(t)
	defer s.Close()
	ctx := context.Background()
	installed, err := agentteams.LoadInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := piproviders.LoadInstalled(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, s.config.HostProfile); err != nil {
		t.Fatal(err)
	}
	s.config.PackageRoot = root
	s.hostProfile, err = readHostProfileIdentity(s.config.HostProfile)
	if err != nil {
		t.Fatal(err)
	}
	s.installedTeams = &installed
	services, err := newPiSelectionServices(Config{Workspace: s.config.Workspace, UserStateRoot: s.config.UserStateRoot, PiAuthDirectory: filepath.Join(t.TempDir(), "auth"), PiHostStateRoot: filepath.Join(t.TempDir(), "host"), PiProviderConfig: &configuration})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &piUsabilityRuntime{piCreationRuntimeFixture: native, receipts: map[string]piruntime.SubmissionStatus{}}
	services.Runtime = runtime
	services.StartRequest = s.config.PiCreation.StartRequest
	s.config.PiCreation = services
	s.config.PiProviderConfig = &configuration
	s.piWorkflows = newPiWorkflowComposition(s)
	s.uploadStore.MinFreeBytes = 0
	scope, err := s.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files := &uploads.Backend{Store: s.uploadStore, ScopeID: scope.ID}
	ids := []string{}
	for i, name := range []string{"source.txt", "archive.bin"} {
		data := []byte("ordinary file")
		if i == 1 {
			data = []byte{0, 1, 2, 3}
		}
		file, err := files.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(700 + i), Name: name, Size: int64(len(data))})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if _, err = files.Append(ctx, file.ID, 0, hex.EncodeToString(sum[:]), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if _, err = files.Complete(ctx, file.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, file.ID)
	}
	named := 0
	s.config.SessionNamer = func(_ context.Context, text, provider string) (string, error) {
		named++
		if text != "Inspect attached inputs" || provider != "" {
			t.Error("naming received wire/files/provider override")
		}
		return "", errors.New("offline naming unavailable")
	}
	frozen := services.providers.preparationProvider()
	fields := url.Values{"backend": {"pi"}, "piProviderDigest": {piPreparationProviderDigest(frozen)}, "team": {"solo"}, "catalogDigest": {installed.Catalog.CatalogDigest}, "model": {"gpt-6.1-sol"}, "effort": {"high"}, "uploadScope": {scope.ID}, "attachmentIds": ids}
	id := preparationTestID(600)
	response := postPreparation(t, s, id, "Inspect attached inputs", "", fields)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	preparation := awaitPreparation(t, s, id)
	if preparation.Schema != 3 || preparation.InputVersion != 3 || preparation.State != "failed" || preparation.Slug != "" || named != 1 || native.sends != 0 {
		t.Fatalf("naming-before-reservation %#v native=%d", preparation, native.sends)
	}
	wire := preparation.Snapshot.Goal
	if !strings.Contains(wire, "source.txt") || !strings.Contains(wire, "archive.bin") {
		t.Fatal("generic reference prompt", wire)
	}
	if err := s.uploadStore.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	nameBody, _ := json.Marshal(map[string]any{"receiptId": preparation.ReceiptID, "attempt": preparation.Attempt, "name": "pi-files"})
	req := httptest.NewRequest("POST", "/api/session-creations/"+id+"/name", bytes.NewReader(nameBody))
	req.Header.Set("Origin", s.config.BaseURL)
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	s.Handler().ServeHTTP(response, req)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := s.preparationStatus(id)
		if status.State == "ready" {
			break
		}
		if status.State == "failed" {
			t.Fatal(status.Error)
		}
		time.Sleep(time.Millisecond)
	}
	status, _ := s.preparationStatus(id)
	if status.State != "ready" || native.sends != 1 || named != 1 {
		t.Fatal("original handoff", status, native.sends, named)
	}
	scopeValue := piWorkflowNamespace(native.state.Conversation)
	dispatcher, err := s.piConversationHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	if err != nil {
		t.Fatal(err)
	}
	s.conversation = dispatcher
	call := func(method, path, body, scope string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", s.config.BaseURL)
		r.Header.Set("Content-Type", "application/json")
		if scope != "" {
			r.Header.Set("X-Workspace-Conversation-Scope", scope)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, route := range []string{"thread", "thread/page", "thread"} {
		path := "/codex/conversations/" + status.Slug + "/" + route
		w := call("GET", path, "", scopeValue)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var transcript backend.Transcript
		if err := json.Unmarshal(w.Body.Bytes(), &transcript); err != nil {
			t.Fatal(err)
		}
		user := transcript.Entries[0]
		if user.Text != wire || user.DisplayText == nil || *user.DisplayText != "Inspect attached inputs" || len(user.Attachments) != 2 || user.ClientUserMessageID == "" || user.ClientUserMessageDigest != piruntime.MessageDigest(wire) || !strings.Contains(transcript.Entries[1].HTML, "<ul>") {
			t.Fatalf("reload wire/presentation %#v", transcript.Entries)
		}
		download := call("GET", user.Attachments[0].DownloadURL, "", "")
		if download.Code != 200 || download.Body.String() != "ordinary file" {
			t.Fatal("scoped download", download.Code)
		}
	}
	if w := call("GET", "/uploads/s-"+status.Slug, "", "pi-workflow-v1:"+strings.Repeat("f", 64)); w.Code < 400 {
		t.Fatal("foreign file scope accepted")
	}
	for _, route := range []string{"pending", "queue"} {
		if w := call("GET", "/codex/conversations/"+status.Slug+"/"+route, "", scopeValue); w.Code < 400 {
			t.Fatal("fake Pi capability", route)
		}
	}
	if native.sends != 1 {
		t.Fatal("reload resent original", native.sends)
	}
	// Later generic inputs reuse the same catalog and exact receipt identity.
	followup := preparationTestID(650)
	payload, _ := json.Marshal(map[string]any{"message": "second input", "clientUserMessageId": followup, "attachmentIds": ids})
	runtime.unknownOnce = true
	if w := call("POST", "/codex/conversations/"+status.Slug+"/message", string(payload), scopeValue); w.Code != 409 {
		t.Fatal("lost receipt must remain Unknown", w.Code, w.Body.String())
	}
	sends := native.sends
	for _, route := range []string{"thread/page", "thread"} {
		w := call("GET", "/codex/conversations/"+status.Slug+"/"+route, "", scopeValue)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var thread backend.Transcript
		if err := json.Unmarshal(w.Body.Bytes(), &thread); err != nil {
			t.Fatal(err)
		}
		user := thread.Entries[2]
		if user.ClientUserMessageID != followup || user.DisplayText == nil || *user.DisplayText != "second input" || len(user.Attachments) != 2 || user.ClientUserMessageDigest != piruntime.MessageDigest(user.Text) {
			t.Fatal("unknown accepted input did not reconcile", user)
		}
	}
	if native.sends != sends || sends != 2 {
		t.Fatal("reload resent unknown input", native.sends)
	}
	runtime.busy = true
	if w := call("DELETE", "/uploads/s-"+status.Slug+"/"+ids[0]+"?confirmed=true", "", scopeValue); w.Code != 409 {
		t.Fatal("busy referenced delete", w.Code, w.Body.String())
	}
	runtime.busy = false
	incomplete, err := files.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(710), Name: "input.pdf", Size: 9})
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := json.Marshal(map[string]any{"message": "unfinished input", "clientUserMessageId": preparationTestID(651), "attachmentIds": []string{incomplete.ID}})
	if w := call("POST", "/codex/conversations/"+status.Slug+"/message", string(bad), scopeValue); w.Code < 400 {
		t.Fatal("unfinished upload submitted")
	}
	if native.sends != 2 {
		t.Fatal("unfinished upload reached native Send")
	}
	if predecessor := os.Getenv("PI_USABILITY_PREDECESSOR_PACKAGE"); predecessor != "" {
		var intent session.PiCreationIntent
		if err := readCreationJSON(s.creationEvidencePath(creationReceipt{ReceiptID: preparation.ReceiptID, Request: creationRequest{Slug: status.Slug}})+".request", &intent); err != nil {
			t.Fatal(err)
		}
		saved := piruntime.StartRequest{Session: intent.SessionRef(), BindingRevision: 1, RequestID: intent.InitialInputID, AppStorePath: root, ProfilePath: s.config.HostProfile, ProfileGeneration: piProfileToken(s.hostProfile)}
		if err := admitPiFrozenPackage(s.config, s.hostProfile, intent, saved); err != nil {
			t.Fatal("control admission", err)
		}
		saved.AppStorePath = predecessor
		if err := admitPiFrozenPackage(s.config, s.hostProfile, intent, saved); !errors.Is(err, piruntime.ErrHeld) {
			t.Fatal("changed contract must keep old pre-native launch held", err)
		}
	}
}
