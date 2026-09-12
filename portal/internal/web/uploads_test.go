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
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
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
