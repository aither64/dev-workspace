package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/uploads"
)

const submissionValidationID = "00000000-0000-4000-8000-000000000001"

type submissionValidationClient struct {
	*browserContractCodex
	sends, queues int
	nativeError   error
	afterQueue    func()
}

func (client *submissionValidationClient) Send(_ context.Context, _, _, id, _ string) (codex.SendReceipt, error) {
	client.sends++
	return codex.SendReceipt{TurnID: "turn-1", ClientUserMessageID: id}, client.nativeError
}

func (client *submissionValidationClient) Queue(_ context.Context, _, text, id string) (codex.QueueEntry, error) {
	client.queues++
	if client.afterQueue != nil {
		client.afterQueue()
	}
	return codex.QueueEntry{ID: "queued-1", Text: text, ClientUserMessageID: id}, client.nativeError
}

type submissionValidationFixture struct {
	server  *Server
	client  *submissionValidationClient
	uploads *uploads.Backend
	file    conversation.Upload
}

func newSubmissionValidationFixture(t *testing.T) *submissionValidationFixture {
	t.Helper()
	server := newTestServer(t)
	prepareInteractiveConversation(t, server, "example")
	client := &submissionValidationClient{browserContractCodex: &browserContractCodex{}}
	server.config.Codex = client
	server.uploadStore.MinFreeBytes = 0
	backend, err := server.sessionUploads(context.Background(), "example", "thread-1", false)
	if err != nil {
		t.Fatal(err)
	}
	file, err := backend.Create(context.Background(), conversation.UploadRequest{
		ClientID: submissionValidationID, Name: "evidence.txt", Size: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("data"))
	if _, err := backend.Append(context.Background(), file.ID, 0, hex.EncodeToString(sum[:]), strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	file, err = backend.Complete(context.Background(), file.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &submissionValidationFixture{server: server, client: client, uploads: backend, file: file}
}

func (fixture *submissionValidationFixture) submit(t *testing.T, endpoint, message, id string, files []string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"message": message, "clientUserMessageId": id, "attachmentIds": files})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/example/"+endpoint, bytes.NewReader(body))
	request.Header.Set("Origin", fixture.server.config.BaseURL)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, request)
	return response
}

func submissionValidationError(t *testing.T, response *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error object: %d %s", response.Code, response.Body.String())
	}
	if response.Code != status || len(body) != 1 || body["error"] != message {
		t.Fatalf("response = %d %s, want %d error %q", response.Code, response.Body.String(), status, message)
	}
}

func TestSubmissionValidationBeforeNativeDispatch(t *testing.T) {
	// These exact status/text pairs support the browser's finite fresh-refusal
	// recovery rule. Exercise the pinned handler and real portal upload backend;
	// a matching error after any native call must never satisfy this contract.
	tests := []struct {
		name, message string
		status        int
		setup         func(*testing.T, *submissionValidationFixture, *string, *[]string, *string)
	}{
		{name: "invalid-attempt", status: 400, message: "message and browser attempt ID are invalid", setup: func(_ *testing.T, _ *submissionValidationFixture, _ *string, _ *[]string, id *string) {
			*id = "invalid"
		}},
		{name: "handler-count", status: 400, message: "too many attachments", setup: func(_ *testing.T, _ *submissionValidationFixture, _ *string, files *[]string, _ *string) {
			*files = make([]string, 101)
		}},
		{name: "invalid-file-id", status: 400, message: "attachment IDs are invalid or repeated", setup: func(_ *testing.T, _ *submissionValidationFixture, _ *string, files *[]string, _ *string) {
			*files = []string{"invalid"}
		}},
		{name: "repeated-file-id", status: 400, message: "attachment IDs are invalid or repeated", setup: func(_ *testing.T, f *submissionValidationFixture, _ *string, files *[]string, _ *string) {
			*files = []string{f.file.ID, f.file.ID}
		}},
		{name: "no-provider", status: 400, message: "attachments are unavailable", setup: func(_ *testing.T, f *submissionValidationFixture, _ *string, _ *[]string, _ *string) {
			f.server.uploadStore = nil
		}},
		{name: "handler-reference-limit", status: 400, message: "message and attachment references exceed the prompt limit", setup: func(t *testing.T, f *submissionValidationFixture, _ *string, _ *[]string, _ *string) {
			// A smaller embedding limit reaches the shared handler's own guard;
			// the portal backend still prepares references under its normal limit.
			var err error
			f.server.conversation, err = conversation.NewHandler(conversation.Options{BasePath: "/codex", AllowedOrigins: f.server.trustedOrigins,
				MaxMessageBytes: 64, Resolver: conversation.ResolverFunc(f.server.resolveConversation)})
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "portal-count", status: 400, message: "Too many files for one prompt", setup: func(_ *testing.T, _ *submissionValidationFixture, _ *string, files *[]string, _ *string) {
			*files = make([]string, uploads.DefaultPromptFiles+1)
			for i := range *files {
				(*files)[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
			}
		}},
		{name: "unknown-file", status: 400, message: "An attachment is unavailable, incomplete or repeated", setup: func(_ *testing.T, _ *submissionValidationFixture, _ *string, files *[]string, _ *string) {
			*files = []string{submissionValidationID}
		}},
		{name: "incomplete-file", status: 400, message: "An attachment is unavailable, incomplete or repeated", setup: func(t *testing.T, f *submissionValidationFixture, _ *string, files *[]string, _ *string) {
			file, err := f.uploads.Create(context.Background(), conversation.UploadRequest{ClientID: "00000000-0000-4000-8000-000000000002", Name: "unfinished.txt", Size: 4})
			if err != nil {
				t.Fatal(err)
			}
			*files = []string{file.ID}
		}},
		{name: "deleted-file", status: 400, message: "An attachment is unavailable, incomplete or repeated", setup: func(t *testing.T, f *submissionValidationFixture, _ *string, _ *[]string, _ *string) {
			if err := f.uploads.Delete(context.Background(), f.file.ID, false); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "attachment-size", status: 413, message: "Attachments exceed the prompt size limit", setup: func(_ *testing.T, f *submissionValidationFixture, _ *string, _ *[]string, _ *string) {
			f.server.uploadStore.UploadLimits.PromptBytes = 3
		}},
		{name: "portal-reference-limit", status: 413, message: "Message and attachment references exceed the prompt limit", setup: func(_ *testing.T, _ *submissionValidationFixture, text *string, _ *[]string, _ *string) {
			*text = strings.Repeat("x", session.MaxMessageBytes-1)
		}},
		{name: "missing-file-bytes", status: 409, message: "An uploaded file is unavailable", setup: func(t *testing.T, f *submissionValidationFixture, _ *string, _ *[]string, _ *string) {
			if err := os.Remove(filepath.Join(f.server.uploadStore.Directory, "files", f.file.ID, "file.txt")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, endpoint := range []string{"message", "queue"} {
		for _, test := range tests {
			t.Run(endpoint+"/"+test.name, func(t *testing.T) {
				fixture := newSubmissionValidationFixture(t)
				text, id, files := "Inspect evidence", submissionValidationID, []string{fixture.file.ID}
				test.setup(t, fixture, &text, &files, &id)
				response := fixture.submit(t, endpoint, text, id, files)
				submissionValidationError(t, response, test.status, test.message)
				if fixture.client.sends != 0 || fixture.client.queues != 0 {
					t.Fatalf("validation reached native calls: send=%d queue=%d", fixture.client.sends, fixture.client.queues)
				}
			})
		}
	}
}

func TestSubmissionFailureAfterNativeDispatch(t *testing.T) {
	for _, endpoint := range []string{"message", "queue"} {
		for _, outcome := range []string{"accepted", "native-error", "identity-conflict"} {
			t.Run(endpoint+"/"+outcome, func(t *testing.T) {
				fixture := newSubmissionValidationFixture(t)
				if outcome == "native-error" {
					// Even a native error carrying an allowlisted UploadError must
					// leave through serverError, never masquerade as pre-dispatch proof.
					fixture.client.nativeError = &conversation.UploadError{Status: 400, Message: "An attachment is unavailable, incomplete or repeated"}
				}
				response := fixture.submit(t, endpoint, "Inspect evidence", submissionValidationID, []string{fixture.file.ID})
				if outcome == "native-error" {
					submissionValidationError(t, response, 503, "Codex service is unavailable")
				} else {
					if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), submissionValidationID) {
						t.Fatalf("valid submission = %d %s", response.Code, response.Body.String())
					}
					if outcome == "identity-conflict" {
						response = fixture.submit(t, endpoint, "Changed text", submissionValidationID, []string{fixture.file.ID})
						submissionValidationError(t, response, 409, "Submission identity was reused with different attachments or text")
					}
				}
				wantSend, wantQueue := 1, 0
				if endpoint == "queue" {
					wantSend, wantQueue = 0, 1
				}
				if fixture.client.sends != wantSend || fixture.client.queues != wantQueue {
					t.Fatalf("native calls = send:%d queue:%d, want send:%d queue:%d", fixture.client.sends, fixture.client.queues, wantSend, wantQueue)
				}
			})
		}
	}
	// The exported delivery errors carry upstream's native admission/ledger
	// proof (codex/send_admission_test.go). Here only the portal HTTP wiring is
	// under test: typed outcomes belong to Send, not Queue or upload validation.
	// The pinned handler includes retryable only for typed nonadmission; an
	// unknown delivery must not gain that field or become a retryable refusal.
	for _, endpoint := range []string{"message", "queue"} {
		for _, outcome := range []struct {
			name, code, message string
			err                 error
			status              int
			notSent, retryable  bool
		}{
			{name: "typed-not-sent", code: "message_not_sent", message: "The message was not sent.",
				err: &codex.MessageNotSentError{Err: fmt.Errorf("private native detail")}, status: 503, notSent: true},
			{name: "typed-not-sent-retryable", code: "message_not_sent", message: "The message was not sent.",
				err: &codex.MessageNotSentError{Err: fmt.Errorf("private native detail"), Retryable: true}, status: 503, notSent: true, retryable: true},
			{name: "typed-unknown", code: "message_delivery_unknown", message: "Message delivery could not be confirmed.",
				err: &codex.UnknownSendOutcomeError{Err: fmt.Errorf("private native detail")}, status: 409},
		} {
			t.Run(endpoint+"/"+outcome.name, func(t *testing.T) {
				fixture := newSubmissionValidationFixture(t)
				fixture.client.nativeError = fmt.Errorf("wrapped native outcome: %w", outcome.err)
				response := fixture.submit(t, endpoint, "Inspect evidence", submissionValidationID, []string{fixture.file.ID})
				if endpoint == "message" {
					var body map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					want := map[string]any{"error": outcome.message, "code": outcome.code, "notSent": outcome.notSent}
					if outcome.notSent {
						want["retryable"] = outcome.retryable
					}
					if response.Code != outcome.status || !reflect.DeepEqual(body, want) {
						t.Fatalf("typed response = %d %s, want %d %v", response.Code, response.Body.String(), outcome.status, want)
					}
				} else {
					submissionValidationError(t, response, 503, "Codex service is unavailable")
				}
				wantSend, wantQueue := 1, 0
				if endpoint == "queue" {
					wantSend, wantQueue = 0, 1
				}
				if fixture.client.sends != wantSend || fixture.client.queues != wantQueue {
					t.Fatalf("typed outcome native calls = send:%d queue:%d, want send:%d queue:%d", fixture.client.sends, fixture.client.queues, wantSend, wantQueue)
				}
			})
		}
	}
	t.Run("queue/post-acceptance-observation", func(t *testing.T) {
		fixture := newSubmissionValidationFixture(t)
		fixture.client.afterQueue = func() {
			// Native acceptance has happened. Fail the real backend's following
			// catalog read without replacing ObserveQueue with a fake provider.
			if err := os.WriteFile(filepath.Join(fixture.server.uploadStore.Directory, "catalog.json"), []byte("invalid catalog"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		response := fixture.submit(t, "queue", "Inspect evidence", submissionValidationID, []string{fixture.file.ID})
		submissionValidationError(t, response, 500, "file operation failed; retry shortly")
		if fixture.client.sends != 0 || fixture.client.queues != 1 {
			t.Fatalf("post-acceptance calls = send:%d queue:%d", fixture.client.sends, fixture.client.queues)
		}
	})
}
