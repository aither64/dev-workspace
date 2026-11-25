package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
)

func TestDraftUploadHTTPAndCreationOwnership(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.uploadStore.MinFreeBytes = 0
	handler := server.Handler()
	call := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Origin", server.config.BaseURL)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	response := call("POST", "/api/upload-drafts", "", nil)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var scope map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &scope); err != nil {
		t.Fatal(err)
	}
	response = call("POST", scope["url"], `{"clientId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","name":"evidence.txt","size":7}`, nil)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var file conversation.Upload
	if err := json.Unmarshal(response.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("content"))
	path := scope["url"] + "/" + file.ID
	response = call("PATCH", path, "content", map[string]string{"Upload-Offset": "0", "Upload-Checksum": hex.EncodeToString(hash[:])})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = call("POST", path, "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = call("GET", path+"/content", "", nil)
	if response.Code != 200 || response.Body.String() != "content" {
		t.Fatal(response.Code, response.Body.String())
	}
	wire, err := server.prepareCreationAttachments(context.Background(), "example", scope["id"], "", []string{file.ID})
	if err != nil || !strings.Contains(wire, "evidence.txt") || strings.Contains(wire, "content") {
		t.Fatal(wire, err)
	}
	// A rejected creation stays editable and expires normally; accepted receipts
	// retain the exact generated prompt independently of the browser response.
	second, err := server.prepareCreationAttachments(context.Background(), "example", scope["id"], "Look at this", []string{file.ID})
	if err != nil || second == wire {
		t.Fatal(second, err)
	}
	if err := server.uploadStore.RetainCreations(context.Background(), map[string]uploads.Scope{wire: {Slug: "example", Epoch: "epoch"}}); err != nil {
		t.Fatal(err)
	}
	response = call("DELETE", path, "", nil)
	if response.Code != 409 {
		t.Fatal("accepted initial input could be deleted", response.Code)
	}
	if err := server.uploadStore.BindCreation(context.Background(), wire, "example", "thread", "epoch"); err != nil {
		t.Fatal(err)
	}
	response = call("GET", path, "", nil)
	if response.Code != 404 {
		t.Fatal("adopted draft remained available", response.Code)
	}
	// Inputs remain outside the workspace and never enter tracking.
	if strings.HasPrefix(server.uploadStore.Directory, server.config.Workspace+"/") {
		t.Fatal("uploads inside workspace")
	}
	var all bytes.Buffer
	response = call("GET", "/uploads/s-unknown", "", nil)
	io.Copy(&all, response.Body)
	if response.Code != 404 {
		t.Fatal(response.Code, all.String())
	}
}

func TestDraftDeletionWaitsForCreationAndReconcilesFailedRetention(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	ctx := context.Background()
	server.uploadStore.MinFreeBytes = 0
	scope, err := server.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID, LockReferences: server.lockUploadReferences}
	file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "input", Size: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	wire, err := server.prepareCreationAttachments(ctx, "example", scope.ID, "", []string{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.uploadCreationMu.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- backend.Delete(ctx, file.ID, false) }()
	select {
	case err := <-deleted:
		server.uploadCreationMu.Unlock()
		t.Fatal("deletion bypassed creation lock", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Acceptance survived but its following catalog update did not. Deletion must
	// recover the durable receipt before considering this prepared draft editable.
	server.operationMu.Lock()
	server.creations["example"] = creationReceipt{Request: creationRequest{Kind: "new", Slug: "example", Goal: wire}, Goal: wire, State: "paused", DeletionHistorySHA256: "epoch"}
	server.operationMu.Unlock()
	server.uploadCreationMu.Unlock()
	err = <-deleted
	var conflict *conversation.UploadError
	if !errors.As(err, &conflict) || conflict.Status != 409 {
		t.Fatal("accepted input deletion was allowed", err)
	}
	content, err := backend.Open(ctx, file.ID)
	if err != nil {
		t.Fatal("accepted bytes removed", err)
	}
	content.File.Close()
}

// Metadata and chunks must cross the same captured Pi conversation boundary.
func TestPiUploadChunksRequireCurrentScopeBeforeAppend(t *testing.T) {
	s, receipt, native := newPiCreationFixtureForSlug(t, "2026-11-17-upload-scope")
	defer s.Close()
	if err := s.initializePiCreation(receipt); err != nil {
		t.Fatal(err)
	}
	runtime := &piNeutralFixture{piCreationRuntimeFixture: native}
	s.config.PiCreation.Runtime = runtime
	s.uploadStore.MinFreeBytes = 0
	beforeSends, beforeCreates, beforeInput := native.sends, native.creates, *native.submission
	handler := s.Handler()
	scope := piWorkflowNamespace(native.state.Conversation)
	base := "/uploads/s-" + receipt.Request.Slug
	call := func(method, path, body, expectedScope string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Origin", s.config.BaseURL)
		if expectedScope != "" {
			request.Header.Set("X-Workspace-Conversation-Scope", expectedScope)
		}
		if method == "PATCH" {
			digest := sha256.Sum256([]byte(body))
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("Upload-Offset", "0")
			request.Header.Set("Upload-Checksum", hex.EncodeToString(digest[:]))
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	created := call("POST", base, `{"clientId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","name":"public.bin","size":7}`, scope)
	if created.Code != 201 {
		t.Fatalf("metadata: %d %s", created.Code, created.Body.String())
	}
	var file conversation.Upload
	if err := json.Unmarshal(created.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	path := base + "/" + file.ID
	status := func() conversation.Upload {
		t.Helper()
		response := call("GET", path, "", scope)
		if response.Code != 200 {
			t.Fatalf("status: %d %s", response.Code, response.Body.String())
		}
		var current conversation.Upload
		if err := json.Unmarshal(response.Body.Bytes(), &current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	before := status()
	for _, test := range []struct {
		scope string
		code  int
	}{
		{"", 409}, {"malformed", 409}, {"pi-workflow-v1:" + strings.Repeat("f", 64), 404},
	} {
		response := call("PATCH", path, "content", test.scope)
		if response.Code != test.code {
			t.Fatalf("scope %q: %d %s", test.scope, response.Code, response.Body.String())
		}
		if current := status(); current.Offset != before.Offset || !reflect.DeepEqual(current.Checksums, before.Checksums) || current.State != before.State {
			t.Fatal("refused scope changed upload progress", current)
		}
	}
	if response := call("PATCH", path, "content", scope); response.Code != 200 {
		t.Fatalf("chunk: %d %s", response.Code, response.Body.String())
	}
	if current := status(); current.Offset != 7 || len(current.Checksums) != 1 {
		t.Fatal("valid append not recorded once", current)
	}
	if response := call("POST", path, "", scope); response.Code != 200 {
		t.Fatalf("complete: %d %s", response.Code, response.Body.String())
	}
	if current := status(); current.State != "ready" {
		t.Fatal("completion", current)
	}
	if native.sends != beforeSends || native.creates != beforeCreates || runtime.ordinarySends != 0 || !reflect.DeepEqual(*native.submission, beforeInput) {
		t.Fatal("file upload created native input or changed its UUID")
	}
}

func TestAcceptedForkUploadsUseSavedCapturedSourceAndRetainLegacyOriginal(t *testing.T) {
	server, _, _, _, statePath := boundRecoveryFixture(t)
	defer server.Close()
	ctx := context.Background()
	server.uploadStore.MinFreeBytes = 0
	selected, err := filepath.EvalSymlinks(server.config.HostProfile)
	if err != nil {
		t.Fatal(err)
	}
	server.config.PackageRoot, server.config.WorkspaceName = selected, "registered"
	contract := filepath.Join(selected, "share/workspace-portal/runtime-contract.json")
	if err := os.MkdirAll(filepath.Dir(contract), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contract, []byte(`{"codexProviderBindingStateSchema":3,"codexProviderForkTransitionPolicy":1,"codexRuntimeAuthoritySchema":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	source, identity, capture, err := server.creationForkSource(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(thread, name, clientID string) (conversation.Upload, string) {
		t.Helper()
		scope, err := server.uploadStore.SessionScope(ctx, "example", thread, "source-epoch")
		if err != nil {
			t.Fatal(err)
		}
		backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID}
		file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: clientID, Name: name, Size: 0})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Complete(ctx, file.ID); err != nil {
			t.Fatal(err)
		}
		wire, err := backend.Prepare(ctx, "send", clientID, name, []string{file.ID})
		if err != nil {
			t.Fatal(err)
		}
		return file, wire
	}
	ancestor, ancestorWire := prepare("thread-1", "ancestor.txt", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	current, currentWire := prepare("root-child", "captured.txt", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	entries := []codex.TranscriptEntry{{Kind: "userMessage", Text: ancestorWire, ItemID: "old-item"}, {Kind: "userMessage", Text: currentWire, ItemID: "captured-item"}}
	server.config.Codex = &browserContractCodex{transcript: codex.Transcript{ThreadID: "created-thread", Entries: entries}}
	epoch, err := session.CompletedRemovalHistory(server.config.Workspace, "destination", server.config.UserStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	receipt := creationReceipt{Schema: 1, Workspace: server.config.Workspace, Request: creationRequest{Kind: "fork", Slug: "destination", Source: "example", SourceThreadID: source.Codex.ThreadID, SourceIdentity: identity, SourceFork: capture}, ReceiptID: strings.Repeat("f", 64), DeletionHistorySHA256: epoch, Attempt: 1, State: "ready", Phase: "ready", Validated: true, Model: "model-1", Effort: "high", StartedAt: "2026-11-25T12:00:00Z", UpdatedAt: "2026-11-25T12:00:00Z"}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations["destination"] = receipt
	writeCreationProof(t, server, receipt) // exact completion after ordinary journal retirement
	if err := server.proveCreation(receipt); err != nil {
		t.Fatal("fixture completion proof", err)
	}
	target, err := server.uploadStore.SessionScope(ctx, "destination", "created-thread", epoch)
	if err != nil {
		t.Fatal(err)
	}
	// The Go command's existing projection may already have completed.
	if err := server.uploadStore.Fork(ctx, "root-child", target, entries); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(server.uploadStore.Directory, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Source inventory and authority are no longer needed for exact completion.
	if err := os.RemoveAll(filepath.Join(server.config.Workspace, "work", "example")); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state = bytes.Replace(state, []byte(`"threadId":"root-child"`), []byte(`"threadId":"later-root"`), 1)
	if err := os.WriteFile(statePath, state, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := server.bindCreationUploads(ctx, receipt); err != nil {
			t.Fatal("saved captured projection", err)
		}
	}
	after, err := os.ReadFile(filepath.Join(server.uploadStore.Directory, "catalog.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("replay changed existing Go upload projection", err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: target.ID}
	if _, err := backend.Status(ctx, current.ID); err != nil {
		t.Fatal("captured file was not retained", err)
	}
	if _, err := backend.Status(ctx, ancestor.ID); err == nil {
		t.Fatal("ancestor file was adopted")
	}
	bad := receipt
	wrong := *capture
	wrong.ThreadID = ""
	bad.Request.SourceFork = &wrong
	if err := server.bindCreationUploads(ctx, bad); err == nil {
		t.Fatal("malformed capture fell back to original source")
	}
	legacy := receipt
	legacy.Request.Slug = "legacy-destination"
	legacy.Request.SourceFork = nil
	legacy.ReceiptID = strings.Repeat("e", 64)
	legacy.DeletionHistorySHA256, err = session.CompletedRemovalHistory(server.config.Workspace, legacy.Request.Slug, server.config.UserStateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.saveCreation(legacy); err != nil {
		t.Fatal(err)
	}
	server.creations[legacy.Request.Slug] = legacy
	writeCreationProof(t, server, legacy)
	if err := server.bindCreationUploads(ctx, legacy); err != nil {
		t.Fatal("legacy source projection changed", err)
	}
	legacyScope, err := server.uploadStore.SessionScope(ctx, legacy.Request.Slug, "created-thread", legacy.DeletionHistorySHA256)
	if err != nil {
		t.Fatal(err)
	}
	legacyBackend := &uploads.Backend{Store: server.uploadStore, ScopeID: legacyScope.ID}
	if _, err := legacyBackend.Status(ctx, ancestor.ID); err != nil {
		t.Fatal("legacy original source was replaced", err)
	}
	if _, err := legacyBackend.Status(ctx, current.ID); err == nil {
		t.Fatal("legacy absence adopted today's capture")
	}
}
