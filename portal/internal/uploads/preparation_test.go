package uploads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
)

func preparationFixture(t *testing.T) (*Store, *Backend, conversation.Upload) {
	t.Helper()
	store, _ := fixture(t)
	scope, err := store.NewDraft(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{Store: store, ScopeID: scope.ID, BaseURL: "/uploads/draft"}
	return store, backend, create(t, backend, "input.txt", []byte("data"))
}

func TestPreparationUploadValidationCompactionDoesNotClaim(t *testing.T) {
	store, backend, file := preparationFixture(t)
	ctx := context.Background()
	id := "00000000-0000-4000-8000-000000000001"
	ids := []string{file.ID}
	unused, err := store.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	readCatalog := func() catalog {
		t.Helper()
		encoded, err := os.ReadFile(filepath.Join(store.Directory, "catalog.json"))
		if err != nil {
			t.Fatal(err)
		}
		var data catalog
		if err := json.Unmarshal(encoded, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := readCatalog()
	if _, exists := before.Scopes[unused.ID]; !exists || len(before.Submissions) != 0 {
		t.Fatal("fixture must have an unused scope and no submissions")
	}
	now := store.Now().Add(8 * 24 * time.Hour)
	store.Now = func() time.Time { return now }
	// No intervening transaction: validation must itself save real compaction.
	if err := store.ValidatePreparation(ctx, id, backend.ScopeID, " raw prompt ", ids); err != nil {
		t.Fatal(err)
	}
	validated := readCatalog()
	if _, exists := validated.Scopes[unused.ID]; exists {
		t.Fatal("validation did not persist unrelated expired-scope compaction")
	}
	if len(validated.Submissions) != 0 {
		t.Fatal("validation persisted ownership before the preparation intent")
	}
	if !validated.Files[file.ID].Updated.Equal(before.Files[file.ID].Updated) {
		t.Fatal("validation persisted a selected-file timestamp update")
	}
	content, err := os.ReadFile(store.filePath(validated.Files[file.ID], false))
	if err != nil || string(content) != "data" {
		t.Fatalf("selected bytes=%q, %v", content, err)
	}
	// Validation leaves the scope editable, including upload and deletion.
	extra := create(t, backend, "editable.txt", []byte("more"))
	if err := backend.Delete(ctx, extra.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	wire, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", ids)
	if err != nil {
		t.Fatal(err)
	}
	claimed := readCatalog()
	entry := claimed.Submissions[backend.ScopeID+"/initial/preparation:"+id]
	if len(claimed.Submissions) != 1 || entry.Scope != backend.ScopeID || entry.Kind != "initial" ||
		entry.Attempt != "preparation:"+id || entry.Text != " raw prompt " || entry.Wire != wire ||
		entry.State != "pending" || len(entry.Files) != 1 || entry.Files[0] != file.ID ||
		!claimed.Files[file.ID].Updated.Equal(now) {
		t.Fatalf("claim did not install exact ownership: %#v", entry)
	}
	if err := store.ValidatePreparation(ctx, id, backend.ScopeID, " raw prompt ", ids); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", ids)
	if err != nil || again != wire {
		t.Fatalf("claim replay=%q, %v", again, err)
	}
	status(t, store.ValidatePreparation(ctx, id, backend.ScopeID, "changed", ids), 409)
	otherID := "00000000-0000-4000-8000-000000000002"
	status(t, store.ValidatePreparation(ctx, otherID, backend.ScopeID, " raw prompt ", ids), 409)
	_, err = store.ClaimPreparation(ctx, otherID, backend.ScopeID, " raw prompt ", ids)
	status(t, err, 409)
	status(t, backend.Delete(ctx, file.ID, true), 409)
}

func TestPreparationUploadClaimPinsAndBlocksEveryMutation(t *testing.T) {
	store, backend, file := preparationFixture(t)
	ctx := context.Background()
	id := "00000000-0000-4000-8000-000000000001"
	wire, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", []string{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", []string{file.ID})
	if err != nil || wire != again {
		t.Fatalf("claim replay = %q, %v", again, err)
	}
	_, err = store.ClaimPreparation(ctx, id, backend.ScopeID, "raw prompt", []string{file.ID})
	status(t, err, 409)
	_, err = store.ClaimPreparation(ctx, "00000000-0000-4000-8000-000000000002", backend.ScopeID, " raw prompt ", []string{file.ID})
	status(t, err, 409)
	newID, _ := newID()
	_, err = backend.Create(ctx, conversation.UploadRequest{ClientID: newID, Name: "other.txt", Size: 1})
	status(t, err, 409)
	_, err = backend.Append(ctx, file.ID, 0, hash([]byte("data")), bytes.NewReader([]byte("data")))
	status(t, err, 409)
	_, err = backend.Complete(ctx, file.ID)
	status(t, err, 409)
	status(t, backend.Delete(ctx, file.ID, true), 409)
	for _, kind := range []string{"initial", "message", "queue"} {
		_, err = backend.Prepare(ctx, kind, "other", "new", []string{file.ID})
		status(t, err, 409)
	}
	store.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if err := store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := backend.Status(ctx, file.ID)
	if err != nil || view.State != "ready" {
		t.Fatalf("accepted input collected: %#v, %v", view, err)
	}
	// This checks the current strict schema-1 reader. Exact baseline execution
	// belongs to test/preparation_compatibility.sh, not this unit test.
	encoded, err := os.ReadFile(filepath.Join(store.Directory, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data catalog
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil || data.Schema != 1 {
		t.Fatalf("schema-1 compatibility: %v", err)
	}
	entry := data.Submissions[backend.ScopeID+"/initial/preparation:"+id]
	if entry.State != "pending" || data.Scopes[backend.ScopeID].Slug != "" {
		t.Fatal("pre-slug input is not retained by existing pending semantics")
	}
	if err := store.BindPreparation(ctx, id, backend.ScopeID, wire, "dated-name", "epoch"); err != nil {
		t.Fatal(err)
	}
	status(t, store.BindPreparation(ctx, id, backend.ScopeID, wire, "other-name", "epoch"), 409)
	if err := store.BindCreation(ctx, wire, "dated-name", "thread", "epoch"); err != nil {
		t.Fatal(err)
	}
	scope, err := store.Scope(ctx, backend.ScopeID)
	if err != nil || scope.Draft || scope.Thread != "thread" {
		t.Fatalf("ownership transfer: %#v %v", scope, err)
	}
	if _, err := backend.Prepare(ctx, "message", "after", "next", nil); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationUploadConcurrentClaimsHaveOneOwner(t *testing.T) {
	store, backend, file := preparationFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := store.ClaimPreparation(context.Background(), id, backend.ScopeID, "same", []string{file.ID})
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			status(t, err, 409)
		}
	}
	if winners != 1 {
		t.Fatalf("claim winners=%d", winners)
	}
}

func TestPreparationUploadEmptySelectionDoesNotClaim(t *testing.T) {
	store, backend, _ := preparationFixture(t)
	if _, err := store.ClaimPreparation(context.Background(), "00000000-0000-4000-8000-000000000001", backend.ScopeID, "plain", nil); err != nil {
		t.Fatal(err)
	}
	id, _ := newID()
	if _, err := backend.Create(context.Background(), conversation.UploadRequest{ClientID: id, Name: "still-editable", Size: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationUploadCompetingStateIsRetainedForRecovery(t *testing.T) {
	store, backend, file := preparationFixture(t)
	id := "00000000-0000-4000-8000-000000000001"
	ctx := context.Background()
	wire, err := store.ClaimPreparation(ctx, id, backend.ScopeID, "raw", []string{file.ID})
	if err != nil {
		t.Fatal(err)
	}
	// An older writer can add this unchanged schema-1 initial submission. The
	// isolated baseline fixture exercises the real preceding implementation.
	if err := store.transaction(ctx, func(data *catalog) (bool, error) {
		data.Submissions[backend.ScopeID+"/initial/old-writer"] = submission{Scope: backend.ScopeID, Kind: "initial", Attempt: "old-writer", Text: "competing", Wire: "competing", Files: []string{file.ID}, State: "prepared"}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.ClaimPreparation(ctx, id, backend.ScopeID, "raw", []string{file.ID})
	status(t, err, 409)
	status(t, store.BindPreparation(ctx, id, backend.ScopeID, wire, "dated-name", "epoch"), 409)
	status(t, store.BindCreation(ctx, wire, "dated-name", "thread", "epoch"), 409)
	if view, err := backend.Status(ctx, file.ID); err != nil || view.State != "ready" {
		t.Fatal("conflict removed selected file")
	}
}

func TestPreparationUploadPublishedClaimReplayConfirmsDurability(t *testing.T) {
	store, backend, file := preparationFixture(t)
	ctx := context.Background()
	id := "00000000-0000-4000-8000-000000000001"
	fail := true
	store.directorySync = func(directory string) error {
		if fail {
			return errors.New("injected post-rename catalog sync")
		}
		dir, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	for n := 0; n < 2; n++ {
		_, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", []string{file.ID})
		if !errors.Is(err, ErrPersistenceUnconfirmed) {
			t.Fatalf("claim=%v", err)
		}
		encoded, err := os.ReadFile(filepath.Join(store.Directory, "catalog.json"))
		var data catalog
		if err != nil || json.Unmarshal(encoded, &data) != nil || data.Submissions[backend.ScopeID+"/initial/preparation:"+id].State != "pending" {
			t.Fatalf("published claim absent: %s %v", encoded, err)
		}
		if _, err := os.Stat(store.filePath(data.Files[file.ID], false)); err != nil {
			t.Fatalf("selected bytes released=%v", err)
		}
	}
	store.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if err := store.Collect(ctx); !errors.Is(err, ErrPersistenceUnconfirmed) {
		t.Fatalf("collector did not abort=%v", err)
	}
	// A surviving pending record also needs confirmation without process-local
	// uncertainty knowledge; unchanged lookup alone cannot establish durability.
	store.catalogUnconfirmed.Store(false)
	if _, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", []string{file.ID}); !errors.Is(err, ErrPersistenceUnconfirmed) {
		t.Fatalf("unchanged replay=%v", err)
	}
	fail = false
	wire, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", []string{file.ID})
	if err != nil || wire == "" {
		t.Fatalf("confirmed claim=%q %v", wire, err)
	}
	again, err := store.ClaimPreparation(ctx, id, backend.ScopeID, " raw prompt ", []string{file.ID})
	if err != nil || again != wire {
		t.Fatalf("confirmed replay=%q %v", again, err)
	}
	if err := store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := backend.Status(ctx, file.ID)
	if err != nil || view.State != "ready" {
		t.Fatalf("retention=%#v %v", view, err)
	}
	status(t, backend.Delete(ctx, file.ID, true), 409)
}
