package workspacecodex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

type loadedRetirementErrorReader struct {
	stage, observed string
	err             error
	cancel          context.CancelFunc
}

func (reader *loadedRetirementErrorReader) fail() error {
	reader.observed = reader.stage
	if reader.cancel != nil {
		reader.cancel()
	}
	return reader.err
}

func (reader *loadedRetirementErrorReader) LoadedThreadIDs(context.Context) ([]string, error) {
	if reader.stage == "list" {
		return nil, reader.fail()
	}
	return []string{"retained-root"}, nil
}

func (reader *loadedRetirementErrorReader) ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error) {
	return codex.ThreadMetadata{}, reader.fail()
}

func TestRetireThreadLoadedObservationFailurePreservesCancellationAndCause(t *testing.T) {
	for _, stage := range []string{"list", "metadata"} {
		for _, kind := range []string{"transport", "other"} {
			for _, canceled := range []bool{false, true} {
				name := stage + "/" + kind + "/active"
				if canceled {
					name = stage + "/" + kind + "/canceled"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					cause := errors.New("fixture observation failure")
					var original error = cause
					var transport *codex.TransportError
					if kind == "transport" {
						transport = &codex.TransportError{Err: cause}
						original = transport
					}
					reader := &loadedRetirementErrorReader{stage: stage, err: original}
					if canceled {
						reader.cancel = cancel
					}
					candidate, found, err := loadedRetirementCandidate(ctx, reader, "retained-root", "/workspace/work/example")
					if reader.observed != stage || found || candidate.ID != "" || !errors.Is(err, cause) {
						t.Fatalf("failed observation changed: stage=%s found=%t candidate=%+v err=%v", reader.observed, found, candidate, err)
					}
					if errors.Is(err, context.Canceled) != canceled {
						t.Fatalf("cancellation category changed: canceled=%t err=%v", canceled, err)
					}
					if !canceled && err != original {
						t.Fatal("independent observation failure changed identity", err)
					}
					var gotTransport *codex.TransportError
					if errors.As(err, &gotTransport) != (transport != nil) || gotTransport != transport {
						t.Fatal("transport classification changed", err)
					}
				})
			}
		}
	}
}

func TestRetireThreadCanceledLoadedObservationKeepsAttemptsAndRefusesArchive(t *testing.T) {
	for _, mode := range []string{"cancel", "metadata-cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, fixture := newLoadedRetirementFixture(t, mode, cancel)
			if err := client.RetireThread(ctx, fixture.root, fixture.cwd, false); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled observation lost its category", err)
			}
			if id, err := client.ThreadOperationAttempt(fixture.cwd); err != nil || id != fixture.root {
				t.Fatal("canceled observation cleared the retained attempt", id, err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			wantReads := 1
			if mode == "metadata-cancel" {
				wantReads = 2
			}
			if fixture.reads != wantReads || fixture.archives != 0 || fixture.archived {
				t.Fatal("canceled observation skipped its stage or archived a thread", fixture.reads, fixture.archives, fixture.methods)
			}
		})
	}
}
