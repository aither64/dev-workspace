package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestSessionPreparationNamingFailureKeepsRequestAndUploads(t *testing.T) {
	for _, mode := range []string{"unavailable", "utility_error", "invalid_output", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			if mode != "unavailable" {
				server.config.SessionNamer = func(context.Context, string, string) (string, error) {
					switch mode {
					case "utility_error":
						return "", errors.New("remote sensitive diagnostic")
					case "timeout":
						return "", context.DeadlineExceeded
					default:
						return `{"name":"invalid"}`, nil
					}
				}
			}
			scope, file := preparationReadyFile(t, server)
			id := preparationTestID(201)
			prompt := " I'd like workspace notifications.\nKeep this request exact. "
			extra := url.Values{"uploadScope": {scope.ID}, "attachmentIds": {file.ID}}
			if response := postPreparation(t, server, id, prompt, "", extra); response.Code != 202 {
				t.Fatal(response.Body.String())
			}
			record := awaitPreparation(t, server, id)
			if record.State != "failed" || record.Phase != "stopped" || record.NamingOutcome != mode || record.Base != "" || record.Slug != "" || record.Handoff != nil {
				t.Fatalf("unexpected failure: %#v", record)
			}
			if record.Error != errSessionNaming.Error() || record.Snapshot.Input.RawPrompt != prompt || !reflect.DeepEqual(record.Snapshot.Input.Attachments, []string{file.ID}) {
				t.Fatal("request changed or unsafe diagnostic exposed")
			}
			if len(server.creations) != 0 {
				t.Fatal("naming failure initialized a session")
			}
			if _, err := os.Stat(filepath.Join(server.config.Workspace, "work", "2026-10-03-i-d-like-workspace-notifications")); !os.IsNotExist(err) {
				t.Fatal("prefix fallback created tracking")
			}
			server.Close()
			restarted, err := New(server.config)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			status, _ := restarted.preparationStatus(id)
			if !status.CanSetName || status.InitialRequest != prompt {
				t.Fatalf("recovery status=%#v", status)
			}
			if response := postPreparation(t, restarted, id, prompt, "", extra); response.Code != 202 {
				t.Fatal("identical replay lost accepted files", response.Body.String())
			}
			response := postCreation(t, restarted, "/api/session-creations/"+id+"/name", fmt.Sprintf(`{"receiptId":%q,"attempt":1,"name":"workspace-notifications"}`, record.ReceiptID), "application/json")
			if response.Code != 202 {
				t.Fatal(response.Body.String())
			}
			result := awaitPreparation(t, restarted, id)
			if result.Base != "workspace-notifications" || result.NamingOutcome != "custom" || result.Slug != "2026-10-03-workspace-notifications" || result.Attempt != 2 || result.InputDigest != record.InputDigest || result.SnapshotDigest != record.SnapshotDigest {
				t.Fatalf("selection changed identity: %#v", result)
			}
			if result.Snapshot.Goal != record.Snapshot.Goal {
				t.Fatal("custom selection lost upload references")
			}
		})
	}
}

func TestSessionPreparationProductionNamingFailureStillCreatesWithOriginalRequest(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.config.NamingSocket = filepath.Join(t.TempDir(), "missing.sock")
	server.config.SessionNamer = server.normalSessionName
	scope, file := preparationReadyFile(t, server)
	prompt := "Preserve the complete request and accepted file."
	extra := url.Values{"uploadScope": {scope.ID}, "attachmentIds": {file.ID}}
	id := preparationTestID(210)
	if response := postPreparation(t, server, id, prompt, "", extra); response.Code != http.StatusAccepted {
		t.Fatal(response.Body.String())
	}
	record := awaitPreparation(t, server, id)
	if record.Base != "development-session" || record.Slug != "2026-10-03-development-session" || record.Handoff == nil || record.NamingOutcome != "utility_error" {
		t.Fatalf("naming failure prevented handoff: %#v", record)
	}
	if record.Snapshot.Input.RawPrompt != prompt || !reflect.DeepEqual(record.Snapshot.Input.Attachments, []string{file.ID}) {
		t.Fatal("fallback changed the request or accepted files")
	}
	server.config.SessionNamer = func(context.Context, string, string) (string, error) {
		t.Error("exact retry repeated naming")
		return "", errors.New("unavailable")
	}
	if replay := postPreparation(t, server, id, prompt, "", extra); replay.Code != http.StatusAccepted {
		t.Fatal(replay.Body.String())
	}
	if current := awaitPreparation(t, server, id); current.Slug != record.Slug || current.ReceiptID != record.ReceiptID {
		t.Fatal("retry replaced the accepted session")
	}
}

func TestSessionPreparationCustomNameCollisionAndLostResponse(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	id := preparationTestID(202)
	if response := postPreparation(t, server, id, "notification request", "", nil); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	record := awaitPreparation(t, server, id)
	if err := os.MkdirAll(filepath.Join(server.config.Workspace, "work", "2026-10-03-occupied"), 0700); err != nil {
		t.Fatal(err)
	}
	selectName := func(attempt int, name string) int {
		return postCreation(t, server, "/api/session-creations/"+id+"/name", fmt.Sprintf(`{"receiptId":%q,"attempt":%d,"name":%q}`, record.ReceiptID, attempt, name), "application/json").Code
	}
	if got := selectName(1, "occupied"); got != 202 {
		t.Fatal(got)
	}
	collision := awaitPreparation(t, server, id)
	if collision.State != "failed" || collision.Slug != "" || collision.Base != "occupied" || collision.Attempt != 2 {
		t.Fatalf("custom collision was suffixed: %#v", collision)
	}
	if got := selectName(1, "occupied"); got != 202 {
		t.Fatalf("lost response replay=%d", got)
	}
	if got := selectName(1, "different"); got != 409 {
		t.Fatalf("stale different selection=%d", got)
	}
	if got := selectName(2, "../escape"); got != 400 {
		t.Fatalf("unsafe name=%d", got)
	}
	if got := selectName(2, "Custom_name"); got != 202 {
		t.Fatal(got)
	}
	result := awaitPreparation(t, server, id)
	if result.Slug != "2026-10-03-Custom_name" || result.Attempt != 3 {
		t.Fatalf("collision continuation=%#v", result)
	}
	if got := selectName(2, "Custom_name"); got != 202 {
		t.Fatalf("reserved replay=%d", got)
	}
	if got := selectName(3, "another-name"); got != 409 {
		t.Fatalf("reserved name changed=%d", got)
	}
}

func TestSessionPreparationNamingRetryUsesOriginalRequest(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	id := preparationTestID(203)
	postPreparation(t, server, id, "original request", "", nil)
	record := awaitPreparation(t, server, id)
	server.config.SessionNamer = func(_ context.Context, prompt string, _ string) (string, error) {
		if prompt != "original request" {
			t.Errorf("prompt changed: %q", prompt)
		}
		return `{"name":"workspace-notifications"}`, nil
	}
	response := postCreation(t, server, "/api/session-creations/"+id+"/retry", fmt.Sprintf(`{"receiptId":%q,"attempt":1}`, record.ReceiptID), "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Body.String())
	}
	result := awaitPreparation(t, server, id)
	if result.NamingOutcome != "model" || result.Slug != "2026-10-03-workspace-notifications" || result.InputDigest != record.InputDigest {
		t.Fatalf("retry=%#v", result)
	}
}

func TestSessionPreparationNameRequiresStoppedUnreservedAutomaticRequest(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	for number, mode := range []string{"running", "custom_input", "saved_model"} {
		t.Run(mode, func(t *testing.T) {
			record := fixturePreparation(t, server, number+300, "failed")
			switch mode {
			case "running":
				record.State = "running"
				server.preparationWork[record.RequestID] = preparationWork{attempt: 1, dispatched: true}
			case "custom_input":
				record.Snapshot.Input.CustomName = "original"
			case "saved_model":
				record.Base, record.NamingOutcome = "saved-model", "model"
			}
			record.InputDigest = inputPreparationDigest(record.Snapshot.Input)
			record.SnapshotDigest = preparationDigest(record.Snapshot)
			if err := server.savePreparation(record); err != nil {
				t.Fatal(err)
			}
			response := postCreation(t, server, "/api/session-creations/"+record.RequestID+"/name", fmt.Sprintf(`{"receiptId":%q,"attempt":1,"name":"replacement"}`, record.ReceiptID), "application/json")
			if response.Code != 409 {
				t.Fatalf("unsafe replacement=%d %s", response.Code, response.Body.String())
			}
		})
	}
	for _, path := range []string{"/api/session-creations/" + preparationTestID(1) + "/name/", "/api/session-creations/" + preparationTestID(1) + "/name/extra"} {
		if validPreparationPath(&url.URL{Path: path}) {
			t.Fatalf("accepted noncanonical route: %s", path)
		}
	}
}

func TestSessionPreparationConcurrentNameChoicesAndUnconfirmedPersistence(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	id := preparationTestID(204)
	postPreparation(t, server, id, "original request", "", nil)
	record := awaitPreparation(t, server, id)
	var failed bool
	server.preparationSync = func(directory string) error {
		if !failed && directory == server.preparationDirectory(false) {
			failed = true
			return errors.New("injected selection sync failure")
		}
		return syncPreparationDirectory(directory)
	}
	url := "/api/session-creations/" + id + "/name"
	body := fmt.Sprintf(`{"receiptId":%q,"attempt":1,"name":"manual-name"}`, record.ReceiptID)
	assertPreparationUnconfirmed(t, postCreation(t, server, url, body, "application/json"), id)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, name := range []string{"manual-name", "other-name"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			response := postCreation(t, server, url, fmt.Sprintf(`{"receiptId":%q,"attempt":1,"name":%q}`, record.ReceiptID, name), "application/json")
			codes <- response.Code
		}(name)
	}
	wg.Wait()
	close(codes)
	seen := map[int]int{}
	for code := range codes {
		seen[code]++
	}
	if seen[202] != 1 || seen[409] != 1 {
		t.Fatalf("concurrent choices=%v", seen)
	}
	result := awaitPreparation(t, server, id)
	if result.Attempt != 2 || result.Base != "manual-name" || result.Slug != "2026-10-03-manual-name" {
		t.Fatalf("published selection repeated or changed: %#v", result)
	}
}

func TestSessionPreparationNameReplayAcrossInitializationAndCompaction(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	record := fixturePreparation(t, server, 205, "handed_off")
	record.Attempt, record.NamingOutcome = 2, "custom"
	record.Base, record.Slug, record.Phase = "workspace-notifications", "2026-10-03-workspace-notifications", "initializing"
	record.Epoch, _ = session.CompletedRemovalHistory(server.config.Workspace, record.Slug, server.config.UserStateRoot)
	request := preparationCreationRequest(record)
	record.Handoff = &request
	if err := server.savePreparation(record); err != nil {
		t.Fatal(err)
	}
	receipt := creationReceipt{Schema: 1, Workspace: record.Workspace, Request: request, ReceiptID: record.ReceiptID, DeletionHistorySHA256: record.Epoch,
		Attempt: 1, State: "failed", Phase: "initializing", Error: "Initialization failed", Validated: true, Goal: record.Snapshot.Goal, StartedAt: record.StartedAt, UpdatedAt: record.UpdatedAt}
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	server.creations[record.Slug] = receipt
	states := map[string]preparationStatus{}
	decode := func(key string, responseCode int, encoded []byte) {
		t.Helper()
		var status preparationStatus
		if json.Unmarshal(encoded, &status) != nil || (responseCode != 200 && responseCode != 202) || status.PreparationAttempt != 2 {
			t.Fatalf("%s projection=%d %s", key, responseCode, encoded)
		}
		states[key] = status
	}
	response := getPreparation(t, server, record.RequestID)
	decode("failure", response.Code, response.Body.Bytes())
	body := fmt.Sprintf(`{"receiptId":%q,"attempt":1,"name":"workspace-notifications"}`, record.ReceiptID)
	response = postCreation(t, server, "/api/session-creations/"+record.RequestID+"/name", body, "application/json")
	decode("failureReplay", response.Code, response.Body.Bytes())
	receipt.State, receipt.Phase, receipt.Error = "ready", "ready", ""
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	writeCreationProof(t, server, receipt)
	server.creations[record.Slug] = receipt
	response = getPreparation(t, server, record.RequestID)
	decode("immediateReady", response.Code, response.Body.Bytes())
	// Initialization may retry independently without changing the accepted name.
	receipt.State, receipt.Phase, receipt.Error, receipt.Attempt = "ready", "ready", "", 3
	if err := server.saveCreation(receipt); err != nil {
		t.Fatal(err)
	}
	writeCreationProof(t, server, receipt)
	server.creations[record.Slug] = receipt
	response = getPreparation(t, server, record.RequestID)
	decode("ready", response.Code, response.Body.Bytes())
	if err := server.retireCreation(receipt); err != nil {
		t.Fatal(err)
	}
	compact, err := readPreparation(server.preparationPath(record.RequestID, true), server.config.Workspace)
	if err != nil || compact.Attempt != 2 || compact.Snapshot != nil {
		t.Fatalf("compacted selection identity=%#v %v", compact, err)
	}
	response = getPreparation(t, server, record.RequestID)
	decode("compacted", response.Code, response.Body.Bytes())
	response = postCreation(t, server, "/api/session-creations/"+record.RequestID+"/name", body, "application/json")
	decode("terminalReplay", response.Code, response.Body.Bytes())
	if len(server.creations) != 0 {
		t.Fatal("terminal replay recreated initialization")
	}
	encoded, _ := json.Marshal(states)
	command := exec.Command("node", "preparation_name_handoff_browser_test.cjs")
	command.Stdin = strings.NewReader(string(encoded))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser handoff contract: %s: %v", output, err)
	}
}
