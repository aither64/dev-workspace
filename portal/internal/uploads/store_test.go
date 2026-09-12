package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
)

func fixture(t *testing.T) (*Store, *Backend) {
	t.Helper()
	root := t.TempDir()
	store, err := New(filepath.Join(root, "state"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	store.MinFreeBytes = 0
	store.UploadLimits = conversation.UploadLimits{FileBytes: 64, PromptBytes: 64, SessionBytes: 96, WorkspaceBytes: 128, ChunkBytes: 4, Files: 10}
	scope, err := store.SessionScope(context.Background(), "example", "thread", "epoch")
	if err != nil {
		t.Fatal(err)
	}
	return store, &Backend{Store: store, ScopeID: scope.ID, BaseURL: "/uploads/s-example", CheckIdle: func(context.Context, []Scope) error { return nil }}
}
func create(t *testing.T, backend *Backend, name string, data []byte) conversation.Upload {
	t.Helper()
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	file, err := backend.Create(context.Background(), conversation.UploadRequest{ClientID: id, Name: name, Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(data); offset += int(backend.Limits().ChunkBytes) {
		chunk := data[offset:min(len(data), offset+int(backend.Limits().ChunkBytes))]
		file, err = backend.Append(context.Background(), file.ID, int64(offset), hash(chunk), bytes.NewReader(chunk))
		if err != nil {
			t.Fatal(err)
		}
	}
	file, err = backend.Complete(context.Background(), file.ID)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func status(t *testing.T, err error, want int) {
	t.Helper()
	var target *conversation.UploadError
	if !errors.As(err, &target) || target.Status != want {
		t.Fatalf("error=%v, want status %d", err, want)
	}
}

func TestChunkRecoveryAndImmutableCompletion(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	id, _ := newID()
	file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: id, Name: "test.log", Size: 6})
	if err != nil {
		t.Fatal(err)
	}
	again, err := backend.Create(ctx, conversation.UploadRequest{ClientID: id, Name: "test.log", Size: 6})
	if err != nil || again.ID != file.ID {
		t.Fatal("creation retry changed identity", err)
	}
	_, err = backend.Append(ctx, file.ID, 0, hash([]byte("wrong")), bytes.NewReader([]byte("abcd")))
	status(t, err, 400)
	file, err = backend.Append(ctx, file.ID, 0, hash([]byte("abcd")), bytes.NewReader([]byte("abcd")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Append(ctx, file.ID, 0, hash([]byte("abcd")), bytes.NewReader([]byte("abcd")))
	status(t, err, 409)
	// Simulate an interrupted append whose bytes reached disk but whose offset
	// did not become durable. A restarted store must use the recorded offset.
	record := fileRecord{ID: file.ID, Name: file.Name}
	f, err := os.OpenFile(store.filePath(record, true), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("uncommitted")
	f.Close()
	restarted, err := New(store.Directory, store.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	restarted.UploadLimits = store.UploadLimits
	backend.Store = restarted
	file, err = backend.Status(ctx, file.ID)
	if err != nil || file.Offset != 4 {
		t.Fatal(file, err)
	}
	_, err = backend.Complete(ctx, file.ID)
	status(t, err, 409)
	_, err = backend.Append(ctx, file.ID, 4, hash([]byte("ef")), bytes.NewReader([]byte("ef")))
	if err != nil {
		t.Fatal(err)
	}
	file, err = backend.Complete(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	content, err := backend.Open(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(content.File)
	content.File.Close()
	if string(result) != "abcdef" {
		t.Fatalf("data=%q", result)
	}
	_, err = backend.Append(ctx, file.ID, 6, hash([]byte("x")), bytes.NewReader([]byte("x")))
	status(t, err, 409)
}

func TestPromptAssociationRetryDeletionAndFork(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	file := create(t, backend, "input.txt", []byte("hello"))
	attempt, _ := newID()
	wire, err := backend.Prepare(ctx, "send", attempt, "", []string{file.ID})
	if err != nil || !bytes.Contains([]byte(wire), []byte(store.Directory)) {
		t.Fatal(wire, err)
	}
	_, err = backend.Prepare(ctx, "send", attempt, "different", []string{file.ID})
	status(t, err, 409)
	err = backend.Delete(ctx, file.ID, true)
	status(t, err, 409)
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{{Kind: "userMessage", ItemID: "item", ClientUserMessageID: attempt, Text: wire, ClientUserMessageDigest: "unchanged"}}}
	if err = backend.ObserveTranscript(ctx, &transcript); err != nil {
		t.Fatal(err)
	}
	entry := transcript.Entries[0]
	if entry.Text != wire || entry.ClientUserMessageDigest != "unchanged" || entry.DisplayText == nil || *entry.DisplayText != "" || len(entry.Attachments) != 1 {
		t.Fatal(entry)
	}
	child, err := store.SessionScope(ctx, "child", "child-thread", "epoch")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Fork(ctx, "thread", child, transcript.Entries); err != nil {
		t.Fatal(err)
	}
	childBackend := &Backend{Store: store, ScopeID: child.ID, BaseURL: "/uploads/s-child", CheckIdle: backend.CheckIdle}
	inherited, err := childBackend.Open(ctx, file.ID)
	if err != nil {
		t.Fatal(err)
	}
	inherited.File.Close()
	err = backend.Delete(ctx, file.ID, false)
	status(t, err, 409)
	backend.CheckIdle = func(_ context.Context, refs []Scope) error {
		if len(refs) != 2 {
			t.Fatalf("references=%d", len(refs))
		}
		return problem(409, "busy")
	}
	status(t, backend.Delete(ctx, file.ID, true), 409)
	backend.CheckIdle = childBackend.CheckIdle
	if err = backend.Delete(ctx, file.ID, true); err != nil {
		t.Fatal(err)
	}
	deleted, err := childBackend.Status(ctx, file.ID)
	if err != nil || deleted.State != "deleted" {
		t.Fatal(deleted, err)
	}
	if _, err = os.Stat(store.filePath(fileRecord{ID: file.ID, Name: file.Name}, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted bytes remain", err)
	}
	retry, err := backend.Prepare(ctx, "send", attempt, "", []string{file.ID})
	if err != nil || retry != wire {
		t.Fatal("receipt retry lost original text", err)
	}
}

func TestQuotasAreReservedAcrossConcurrentRequests(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	store.UploadLimits.FileBytes = 64
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _ := newID()
			_, err := backend.Create(ctx, conversation.UploadRequest{ClientID: id, Name: "large", Size: 48})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			status(t, err, 413)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted=%d", accepted)
	}
	other, _ := store.NewDraft(ctx)
	otherBackend := &Backend{Store: store, ScopeID: other.ID}
	id, _ := newID()
	_, err := otherBackend.Create(ctx, conversation.UploadRequest{ClientID: id, Name: "over-workspace", Size: 48})
	status(t, err, 413)
	store.MinFreeBytes = ^uint64(0) / 2
	id, _ = newID()
	_, err = otherBackend.Create(ctx, conversation.UploadRequest{ClientID: id, Name: "disk", Size: 1})
	status(t, err, 507)
}

func TestExpirationScopeIsolationAndExplicitSessionRemoval(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	empty := create(t, backend, "empty", nil)
	sent := create(t, backend, "same-name", []byte("sent"))
	draft := create(t, backend, "same-name", []byte("temp"))
	outsider, _ := store.NewDraft(ctx)
	foreign := &Backend{Store: store, ScopeID: outsider.ID}
	_, err := foreign.Status(ctx, sent.ID)
	status(t, err, 404)
	attempt, _ := newID()
	wire, err := backend.Prepare(ctx, "queue", attempt, "Use input", []string{sent.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	store.Now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	if err = store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{empty.ID, draft.ID} {
		file, err := backend.Status(ctx, id)
		if err == nil && file.State != "deleted" {
			t.Fatal("draft did not expire")
		}
	}
	file, _ := backend.Status(ctx, sent.ID)
	if file.State != "ready" {
		t.Fatal("pending submission expired")
	}
	queue := []codex.QueueEntry{{ID: "queue", Text: wire, ClientUserMessageID: attempt}}
	if err = backend.ObserveQueue(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if err = backend.QueueDeleted(ctx, "queue"); err != nil {
		t.Fatal(err)
	}
	if err = store.RemoveSession(ctx, "example", "different-thread", "epoch"); err != nil {
		t.Fatal(err)
	}
	// Reuse of a slug cannot make the old scope own the replacement thread.
	replacement, _ := store.SessionScope(ctx, "example", "replacement", "new-epoch")
	if replacement.ID == backend.ScopeID {
		t.Fatal("scope was reused")
	}
	if err = store.RemoveSession(ctx, "example", "thread", "epoch"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Scope(ctx, backend.ScopeID); err == nil {
		t.Fatal("deleted owner still accessible")
	}
}

func TestInitialDraftExpiryAcceptanceAndDeletionEpoch(t *testing.T) {
	store, _ := fixture(t)
	ctx := context.Background()
	draft, _ := store.NewDraft(ctx)
	backend := &Backend{Store: store, ScopeID: draft.ID}
	rejected := create(t, backend, "rejected", []byte("no"))
	if _, err := backend.Prepare(ctx, "initial", "first", "", []string{rejected.ID}); err != nil {
		t.Fatal(err)
	}
	accepted := create(t, backend, "accepted", []byte("yes"))
	wire, err := backend.Prepare(ctx, "initial", "second", "", []string{accepted.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RetainCreations(ctx, map[string]Scope{wire: {Slug: "creating", Epoch: "before"}}); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	store.Now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	if err = store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	file, err := backend.Status(ctx, rejected.ID)
	if err == nil && file.State != "deleted" {
		t.Fatal("unaccepted initial draft did not expire")
	}
	file, _ = backend.Status(ctx, accepted.ID)
	if file.State != "ready" {
		t.Fatal("accepted initial input expired")
	}
	if err = store.RemoveSession(ctx, "creating", "thread", "other-epoch"); err != nil {
		t.Fatal(err)
	}
	file, _ = backend.Status(ctx, accepted.ID)
	if file.State != "ready" {
		t.Fatal("foreign deletion removed pending creation")
	}
	if err = store.RemoveSession(ctx, "creating", "thread", "before"); err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Status(ctx, accepted.ID); err == nil {
		t.Fatal("deleted draft remained accessible")
	}
}

func TestForkRecoversUnobservedSourceSubmission(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	file := create(t, backend, "input", []byte("abcd"))
	attempt, _ := newID()
	wire, err := backend.Prepare(ctx, "send", attempt, "review", []string{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := store.SessionScope(ctx, "child", "child-thread", "epoch")
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{{Kind: "userMessage", ItemID: "inherited", Text: wire}}}
	if err = store.Fork(ctx, "thread", child, transcript.Entries); err != nil {
		t.Fatal(err)
	}
	inherited := &Backend{Store: store, ScopeID: child.ID}
	if err = inherited.ObserveTranscript(ctx, &transcript); err != nil {
		t.Fatal(err)
	}
	if len(transcript.Entries[0].Attachments) != 1 {
		t.Fatal("fork lost attachments before source acknowledgement")
	}
}

func TestCatalogReclaimsExpiredMetadataAfterRestart(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	file := create(t, backend, "rejected", []byte("input"))
	if len(file.Checksums) != 0 {
		t.Fatal("completed upload retained chunk hashes")
	}
	if _, err := backend.Prepare(ctx, "initial", "rejected", "", []string{file.ID}); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	if err := store.transaction(ctx, func(data *catalog) (bool, error) {
		for len(data.Scopes) < maxRecords {
			id, _ := newID()
			data.Scopes[id] = Scope{ID: id, Draft: true, Created: now}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewDraft(ctx); err == nil {
		t.Fatal("record admission limit bypassed")
	}
	restarted, err := New(store.Directory, store.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	if err := restarted.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.transaction(ctx, func(data *catalog) (bool, error) {
		if len(data.Files) != 0 || len(data.Submissions) != 0 || len(data.Scopes) != 0 {
			t.Fatalf("obsolete metadata retained: %d files, %d submissions, %d scopes", len(data.Files), len(data.Submissions), len(data.Scopes))
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.NewDraft(ctx); err != nil {
		t.Fatal("capacity not recovered", err)
	}
	backend.Store = restarted
	if _, err := backend.Prepare(ctx, "initial", "rejected", "", []string{file.ID}); err == nil {
		t.Fatal("expired prepared input was replayed")
	}
	if _, err := os.Stat(store.filePath(fileRecord{ID: file.ID, Name: file.Name}, false)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired bytes retained", err)
	}
}

func TestConflictedCreationReleasesDraftAndCanBeRetried(t *testing.T) {
	store, _ := fixture(t)
	ctx := context.Background()
	scope, _ := store.NewDraft(ctx)
	backend := &Backend{Store: store, ScopeID: scope.ID}
	file := create(t, backend, "input", nil)
	wire, err := backend.Prepare(ctx, "initial", "attempt", "", []string{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetainCreations(ctx, map[string]Scope{wire: {Slug: "conflict", Epoch: "epoch"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RetainCreations(ctx, map[string]Scope{wire: {Slug: "conflict", Epoch: "epoch", Deleted: true}}); err != nil {
		t.Fatal(err)
	}
	scope, err = store.Scope(ctx, scope.ID)
	if err != nil || scope.Slug != "" {
		t.Fatal("conflicted draft remains bound", scope, err)
	}
	if _, err := backend.Prepare(ctx, "initial", "attempt", "", []string{file.ID}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(ctx, file.ID, false); err != nil {
		t.Fatal("conflicted file remains pinned", err)
	}
	if _, err := backend.Prepare(ctx, "initial", "attempt", "", []string{file.ID}); err == nil {
		t.Fatal("removed prepared input replayed")
	}
}

func TestSessionRemovalRequiresThreadForExistingOwner(t *testing.T) {
	store, backend := fixture(t)
	file := create(t, backend, "input", nil)
	if err := store.RemoveSession(context.Background(), "example", "", "epoch"); err == nil {
		t.Fatal("removal finalized without thread proof")
	}
	content, err := backend.Open(context.Background(), file.ID)
	if err != nil {
		t.Fatal("failed removal changed file", err)
	}
	content.File.Close()
}

func TestMetadataByteLimitRetainsRecoveryHeadroom(t *testing.T) {
	store, backend := fixture(t)
	ctx := context.Background()
	file := create(t, backend, "input", nil)
	// Seed a realistic-sized sent history close to admission capacity without
	// quadratic fixture writes. All entries belong to the same removable session.
	if err := store.transaction(ctx, func(data *catalog) (bool, error) {
		for i := 0; i < 740; i++ {
			id, _ := newID()
			data.Submissions[id] = submission{Scope: backend.ScopeID, Kind: "send", Attempt: id, Text: strings.Repeat("x", 10000), Wire: strings.Repeat("y", 10000), Files: []string{file.ID}, State: "observed"}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Additional metadata is rejected before it consumes the recovery reserve.
	err := store.transaction(ctx, func(data *catalog) (bool, error) {
		for i := 0; i < 80; i++ {
			id, _ := newID()
			data.Submissions[id] = submission{Scope: backend.ScopeID, Text: strings.Repeat("z", 10000)}
		}
		return true, nil
	})
	status(t, err, 507)
	restarted, err := New(store.Directory, store.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.RemoveSession(ctx, "example", "thread", "epoch"); err != nil {
		t.Fatal("history capacity prevented deletion", err)
	}
	if err := restarted.transaction(ctx, func(data *catalog) (bool, error) {
		if len(data.Submissions) != 0 || len(data.Files) != 0 {
			t.Fatal("deleted history retained")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.NewDraft(ctx); err != nil {
		t.Fatal("history capacity did not recover", err)
	}
}
