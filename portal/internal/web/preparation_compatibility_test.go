//go:build preparation_compatibility

package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
)

// test/preparation_compatibility.sh owns the temporary directory and executes
// the exact baseline reader/writer between these two separate test processes.
type preparationCompatibilityFixture struct {
	Workspace, Directory, ScopeID, FileID, RequestID, ReceiptID string
}

func compatibilityServer(t *testing.T, initialize bool) (*Server, string) {
	t.Helper()
	root := os.Getenv("PREPARATION_FIXTURE_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("PREPARATION_FIXTURE_ROOT must name the isolated absolute test directory")
	}
	workspace, authority := filepath.Join(root, "workspace"), filepath.Join(root, "authority")
	profile, packagePath := filepath.Join(root, "profile"), filepath.Join(root, "package")
	if initialize {
		for _, directory := range []string{workspace, authority, packagePath} {
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(packagePath, profile); err != nil {
			t.Fatal(err)
		}
	}
	server, err := New(Config{Workspace: workspace, BaseURL: "https://workspace.example.test", DevSession: "/does-not-run/dev-session",
		HostProfile: profile, AuthorityDir: authority, CodexSocket: "/run/test/codex.sock", CodexVersion: "0.152.1",
		UserStateRoot: filepath.Join(root, "state"), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	return server, root
}

func TestPreparationCompatibilityFixtureWrite(t *testing.T) {
	server, root := compatibilityServer(t, true)
	defer server.Close()
	ctx := context.Background()
	server.uploadStore.MinFreeBytes = 0
	scope, err := server.uploadStore.NewDraft(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: scope.ID}
	file, err := backend.Create(ctx, conversation.UploadRequest{ClientID: preparationTestID(999), Name: "input.txt", Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("data"))
	if _, err := backend.Append(ctx, file.ID, 0, hex.EncodeToString(digest[:]), bytes.NewReader([]byte("data"))); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Complete(ctx, file.ID); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	id := preparationTestID(1)
	response := postPreparation(t, server, id, " raw compatibility input ", "", url.Values{"uploadScope": {scope.ID}, "attachmentIds": {file.ID}})
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("naming did not start")
	}
	server.Close()
	record, err := readPreparation(server.preparationPath(id, false), server.config.Workspace)
	if err != nil || record.State != "paused" || record.Slug != "" {
		t.Fatalf("fixture=%#v %v", record, err)
	}
	fixture := preparationCompatibilityFixture{server.config.Workspace, server.uploadStore.Directory, scope.ID, file.ID, id, record.ReceiptID}
	encoded, _ := json.Marshal(fixture)
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPreparationCompatibilityFixtureRecover(t *testing.T) {
	server, root := compatibilityServer(t, false)
	defer server.Close()
	encoded, err := os.ReadFile(filepath.Join(root, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture preparationCompatibilityFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	status, ok := server.preparationStatus(fixture.RequestID)
	if !ok || status.State != "failed" || status.ReceiptID != fixture.ReceiptID || status.CanonicalURL != "" || !strings.Contains(status.Error, "ownership changed") {
		t.Fatalf("old competing writer was not detected: %#v", status)
	}
	backend := &uploads.Backend{Store: server.uploadStore, ScopeID: fixture.ScopeID}
	content, err := backend.Open(context.Background(), fixture.FileID)
	if err != nil {
		t.Fatal("selected input lost", err)
	}
	data, err := io.ReadAll(content.File)
	content.File.Close()
	if err != nil || string(data) != "data" {
		t.Fatalf("selected bytes=%q %v", data, err)
	}
	response := postPreparation(t, server, fixture.RequestID, " raw compatibility input ", "", url.Values{"uploadScope": {fixture.ScopeID}, "attachmentIds": {fixture.FileID}})
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	body := fmt.Sprintf(`{"receiptId":%q,"attempt":%d}`, fixture.ReceiptID, status.Attempt)
	response = postCreation(t, server, "/api/session-creations/"+fixture.RequestID+"/retry", body, "application/json")
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	record := awaitPreparation(t, server, fixture.RequestID)
	if record.State != "failed" || record.Base != "" || record.Slug != "" {
		t.Fatal("competing state reached naming/reservation")
	}
	// Both the accepted pending claim and the old writer's competing state stay
	// present for explicit recovery; the new reader never deletes by goal digest.
	encoded, err = os.ReadFile(filepath.Join(fixture.Directory, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte("preparation:"+fixture.RequestID)) || !bytes.Contains(encoded, []byte("old-writer")) {
		t.Fatal("competing evidence was discarded")
	}
}
