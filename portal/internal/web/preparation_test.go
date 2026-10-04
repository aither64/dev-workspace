package web

import (
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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
)

func preparationTestID(number int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", number)
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

func TestSessionNameDeterministicFallbackAndValidation(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"\n  Příliš žluťoučký KŮŇ opravil přenos souborů sedmý\nignored", "prilis-zlutoucky-kun-opravil-prenos-souboru"},
		{"cafe\u0301 numéro 42", "cafe-numero-42"}, {"東京 / łódź", "odz"},
		{"  \n 🔧 \nsecond line", "session"}, {strings.Repeat("x", 49) + " short", "session"},
		{"alpha beta gamma delta epsilon zeta eta", "alpha-beta-gamma-delta-epsilon-zeta"},
	} {
		if got := fallbackSessionName(test.raw); got != test.want {
			t.Errorf("fallback(%q)=%q, want %q", test.raw, got, test.want)
		}
	}
	for _, output := range []string{`{"name":"one-two-three"}`, `{"name":"one-two-three-four-five-six"}`} {
		if _, err := parseSessionName(output); err != nil {
			t.Error(err)
		}
	}
	for _, output := range []string{`{"name":"one-two"}`, `{"name":"One-two-three"}`, `{"name":"one--two-three"}`, `{"name":"one-two-three","extra":0}`, `{"name":"one-two-three","name":"four-five-six"}`, `{"name":"one-two-three"} {}`, `null`, `[]`} {
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

func TestSessionNameAdapterFallbackAndWorkspaceConcurrency(t *testing.T) {
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
		server.config.SessionNamer = func(context.Context, string) (string, error) { return test.output, test.err }
		name, outcome, err := server.namePreparation(context.Background(), "Příliš short prompt")
		if err != nil || name != "prilis-short-prompt" || outcome != test.outcome {
			t.Fatalf("fallback=%q %q %v", name, outcome, err)
		}
	}
	entered, release := make(chan time.Time, 3), make(chan struct{})
	var active, peak atomic.Int32
	server.config.SessionNamer = func(ctx context.Context, prompt string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second || len(prompt) > 8192 {
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
			if _, _, err := server.namePreparation(context.Background(), strings.Repeat("a", 8191)+"ž"); err != nil {
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
	server.config.SessionNamer = func(context.Context, string) (string, error) {
		t.Fatal("attachment-only prompt invoked adapter")
		return "", nil
	}
	if name, outcome, err := server.namePreparation(context.Background(), " \n "); err != nil || name != "session" || outcome != "attachment_only" {
		t.Fatal(name, outcome, err)
	}
}

func TestSessionNameTimeoutIncludesSemaphoreQueue(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.namingSlots <- struct{}{}
	server.namingSlots <- struct{}{}
	server.config.SessionNamer = func(context.Context, string) (string, error) {
		t.Fatal("queued timeout invoked adapter")
		return "", nil
	}
	started := time.Now()
	name, outcome, err := server.namePreparation(context.Background(), "queued naming prompt")
	if err != nil || name != "queued-naming-prompt" || outcome != "timeout" {
		t.Fatal(name, outcome, err)
	}
	if elapsed := time.Since(started); elapsed < 9*time.Second || elapsed > 12*time.Second {
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
	server.config.SessionNamer = func(ctx context.Context, prompt string) (string, error) {
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
	restarted.config.SessionNamer = func(context.Context, string) (string, error) { return `{"name":"fifty-file-recovery"}`, nil }
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

func TestSessionPreparationAdmissionReplaysBeforeCatalogAndNaming(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, prompt string) (string, error) {
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
	server.config.SessionNamer = func(context.Context, string) (string, error) { return `{"name":"same-prompt-work"}`, nil }
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
	server.config.SessionNamer = func(context.Context, string) (string, error) { calls.Add(1); return `{"name":"locked-name-work"}`, nil }
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
		`/static/preparation.js?v=3`, `/static/creation.js?v=3`,
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
	if strings.Contains(body, `name="name" required`) || !strings.Contains(body, `name="name" maxlength="48" pattern="[A-Za-z0-9][A-Za-z0-9_-]*"`) {
		t.Fatal("New session must permit an empty name and retain explicit-name limits")
	}
	prompt, options, name := strings.Index(body, `name="goal"`), strings.Index(body, `<summary>Options</summary>`), strings.Index(body, `name="name"`)
	if prompt < 0 || options < prompt || name < options {
		t.Fatal("initial request must precede optional settings")
	}
	if !strings.Contains(body, `id="new-session-recover"`) || !strings.Contains(body, `/static/preparation.js?v=3`) {
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
			server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
			if err := server.reconcilePreparations(); !errors.Is(err, errPreparationPersistenceUnconfirmed) {
				t.Fatalf("collection barrier=%v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("naming started before confirmation")
			}
			fault.Store(false)
			// Collection may confirm bytes, but retains the pending launch for POST.
			if err := server.reconcilePreparations(); err != nil {
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
			server.config.SessionNamer = func(context.Context, string) (string, error) { calls.Add(1); return `{"name":"one-two-three"}`, nil }
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
	server.config.SessionNamer = func(context.Context, string) (string, error) { calls.Add(1); return `{"name":"one-two-three"}`, nil }
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
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
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
