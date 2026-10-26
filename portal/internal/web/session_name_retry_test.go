package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
)

func TestSessionPreparationAutomaticRetryKeepsOneAcceptedAttempt(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	providerTestCatalog(t, server, "http://127.0.0.1:1")
	transient := errors.New("test settled transient failure")
	calls := 0
	client := ephemeralNamingMock{run: func(_ context.Context, options codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
		calls++
		if options.Input != "Mitigate PHP session upload progress" || options.ModelProvider != "pool" {
			t.Fatal("automatic retry changed accepted input/provider")
		}
		if calls == 1 {
			return codex.EphemeralTurnResult{}, transient
		}
		return codex.EphemeralTurnResult{Text: `{"name":"php-upload-progress-mitigation"}`}, nil
	}}
	server.config.SessionNamer = func(ctx context.Context, input, provider string) (string, error) {
		return server.modelSessionNameWithRetry(ctx, input, provider, client, func(err error) bool { return err == transient })
	}
	id := preparationTestID(212)
	response := postPreparation(t, server, id, "Mitigate PHP session upload progress", "", url.Values{"provider": {"pool"}})
	if response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	record := awaitPreparation(t, server, id)
	if calls != 2 || record.Attempt != 1 || record.Slug != "2026-10-03-php-upload-progress-mitigation" || record.NamingOutcome != "model" || record.Handoff == nil {
		t.Fatalf("automatic retry repeated preparation: calls=%d record=%#v", calls, record)
	}
}

// Real retry metadata is private to codex-web and tested there. Only this test
// predicate substitutes for its accessor; production passes the accessor itself.
func TestSessionNamingRetryKeepsDeadlineProviderInputsAndFreshScratch(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	providerTestCatalog(t, server, "http://127.0.0.1:1")
	statePath, err := codexproviders.StatePath(server.config.UserStateRoot, server.config.WorkspaceName)
	if err != nil {
		t.Fatal(err)
	}
	writeProvider := func(provider string) {
		t.Helper()
		state := codexproviders.State{Schema: 1, Workspace: server.config.WorkspaceName, Root: server.config.Workspace, Socket: server.config.CodexSocket, Active: provider}
		encoded, _ := json.Marshal(state)
		if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProvider("openai")
	provider, err := server.sessionNamingProvider("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	transient := errors.New("test settled transient failure")
	var first codex.EphemeralTurnOptions
	var previous string
	var failedAt time.Time
	calls := 0
	client := ephemeralNamingMock{run: func(call context.Context, options codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
		calls++
		if got, _ := call.Deadline(); got != deadline {
			t.Fatal("retry restarted the original deadline")
		}
		if options.ModelProvider != "openai" || options.Input != "Mitigate PHP session upload progress" {
			t.Fatalf("accepted provider/input changed: %#v", options)
		}
		if calls == 1 {
			first, previous = options, filepath.Dir(options.Directory)
			writeProvider("pool")
			failedAt = time.Now()
			return codex.EphemeralTurnResult{}, transient
		}
		if time.Since(failedAt) < sessionNamingRetryBackoff {
			t.Fatal("retry skipped backoff")
		}
		if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("previous scratch cleanup did not settle: %v", err)
		}
		if options.Directory == first.Directory || options.InstructionFile == first.InstructionFile {
			t.Fatal("retry reused private state")
		}
		first.Directory, first.InstructionFile = options.Directory, options.InstructionFile
		if !reflect.DeepEqual(first, options) {
			t.Fatal("retry changed utility settings")
		}
		return codex.EphemeralTurnResult{Text: `{"name":"php-upload-progress-mitigation"}`}, nil
	}}
	output, err := server.modelSessionNameWithRetry(ctx, "Mitigate PHP session upload progress", provider, client, func(err error) bool { return err == transient })
	if err != nil || output != `{"name":"php-upload-progress-mitigation"}` || calls != 2 {
		t.Fatalf("output=%q calls=%d err=%v", output, calls, err)
	}
}

func TestSessionNamingRetryCapAndAdmission(t *testing.T) {
	transient := errors.New("test settled transient failure")
	for _, test := range []struct {
		name, output string
		err          error
		budget       time.Duration
		calls        int
	}{
		{name: "immediate success", output: `{"name":"php-upload-progress"}`, budget: 2 * time.Second, calls: 1},
		{name: "persistent transient", err: transient, budget: 2 * time.Second, calls: 2},
		{name: "insufficient inference and cleanup", err: transient, budget: 1400 * time.Millisecond, calls: 1},
		{name: "bare server category", err: codex.ErrEphemeralServer, budget: 2 * time.Second, calls: 1},
		{name: "permanent settings", err: codex.ErrEphemeralSettings, budget: 2 * time.Second, calls: 1},
		{name: "invalid output", output: `{"name":"invalid"}`, budget: 2 * time.Second, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), test.budget)
			defer cancel()
			calls := 0
			client := ephemeralNamingMock{run: func(context.Context, codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
				calls++
				return codex.EphemeralTurnResult{Text: test.output}, test.err
			}}
			output, err := server.modelSessionNameWithRetry(ctx, "raw request", "openai", client, func(err error) bool { return err == transient })
			if calls != test.calls || (err == nil) != (test.name == "immediate success") {
				t.Fatalf("calls=%d output=%q err=%v", calls, output, err)
			}
		})
	}
	if codex.CanRetryEphemeralTurn(codex.ErrEphemeralServer) {
		t.Fatal("unproved server failure became retryable")
	}
}

func TestSessionNamingRetryCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entered := make(chan struct{})
	go func() {
		<-entered
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	calls := 0
	_, err := retrySessionName(ctx, func(context.Context) (string, error) {
		calls++
		close(entered)
		return "", errors.New("settled failure")
	}, func(error) bool { return true })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestSessionNamingOperationSlotHeldAcrossRetryAndCleanup(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	server.namingSlots <- struct{}{}
	secondCall := make(chan struct{})
	release := make(chan struct{})
	server.config.SessionNamer = func(ctx context.Context, input, _ string) (string, error) {
		if input != "first request" {
			t.Error("another operation acquired the held retry slot")
			return "", errors.New("unexpected operation")
		}
		calls := 0
		return retrySessionName(ctx, func(context.Context) (string, error) {
			calls++
			if len(server.namingSlots) != 2 {
				t.Error("operation released its slot during retry")
			}
			if calls == 1 {
				return "", errors.New("settled transient")
			}
			close(secondCall)
			<-release // Simulates helper cleanup before returning its result.
			return `{"name":"first-request"}`, nil
		}, func(error) bool { return true })
	}
	finished := make(chan error, 1)
	go func() {
		_, _, err := server.namePreparation(context.Background(), "first request", "")
		finished <- err
	}()
	<-secondCall
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, _, err := server.namePreparation(ctx, "queued request", "")
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation=%v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	<-server.namingSlots
}
