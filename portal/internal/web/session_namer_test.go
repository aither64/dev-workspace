package web

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
)

type ephemeralNamingMock struct {
	run func(context.Context, codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error)
}

func (mock ephemeralNamingMock) RunEphemeralTurn(ctx context.Context, options codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
	return mock.run(ctx, options)
}

func namingTestStore(t *testing.T) *lifecycleOperationStore {
	t.Helper()
	store, err := newLifecycleOperationStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSessionNamingUtilityAdapterExactInputsAndPrivateState(t *testing.T) {
	store := namingTestStore(t)
	raw := strings.Repeat("a", 8191) + "ž attachment:/must/not/be/used"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var private string
	var previous string
	client := ephemeralNamingMock{run: func(call context.Context, options codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
		private = filepath.Dir(options.Directory)
		if private == previous {
			t.Fatal("adapter reused another call's instruction path")
		}
		if got, _ := call.Deadline(); got != deadline {
			t.Fatal("adapter restarted deadline")
		}
		if options.Model != "gpt-6-luna" || options.Effort != "low" || options.Input != strings.Repeat("a", 8191) || !utf8.ValidString(options.Input) {
			t.Fatalf("settings/input=%#v", options)
		}
		if options.Instructions != sessionNamingInstructions || string(options.OutputSchema) != sessionNamingSchema {
			t.Fatal("policy/schema changed")
		}
		var schema map[string]any
		if json.Unmarshal(options.OutputSchema, &schema) != nil {
			t.Fatal("invalid schema")
		}
		entries, err := os.ReadDir(options.Directory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("cwd=%v err=%v", entries, err)
		}
		cwd, _ := os.Stat(options.Directory)
		if cwd.Mode().Perm() != 0700 {
			t.Fatal("cwd not private")
		}
		if options.InstructionFile != filepath.Join(private, "instructions.txt") {
			t.Fatal("instruction override path is not separate per-call state")
		}
		file, err := os.Lstat(options.InstructionFile)
		if err != nil || !file.Mode().IsRegular() || file.Mode().Perm() != 0600 || file.Size() != int64(len(codex.EphemeralInstructionFileContent)) {
			t.Fatal("instruction override not exact/private")
		}
		owner, ok := file.Sys().(*syscall.Stat_t)
		content, readErr := os.ReadFile(options.InstructionFile)
		if !ok || owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 || readErr != nil || string(content) != codex.EphemeralInstructionFileContent {
			t.Fatal("instruction override bytes or ownership changed")
		}
		relative, err := filepath.Rel(store.workspace, options.Directory)
		if err != nil || !strings.HasPrefix(relative, "../") {
			t.Fatal("cwd inside workspace")
		}
		return codex.EphemeralTurnResult{Text: `{"name":"fix runtime session names"}`}, nil
	}}
	for call := 0; call < 2; call++ {
		result, err := modelSessionName(ctx, raw, client, store)
		if err != nil || result != `{"name":"fix runtime session names"}` {
			t.Fatalf("result=%q err=%v", result, err)
		}
		if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch state retained: %v", err)
		}
		previous = private
	}
}

func TestSessionNamingUtilityCleanupAndCancellation(t *testing.T) {
	for _, mode := range []string{"failure", "cancel", "oversized output"} {
		t.Run(mode, func(t *testing.T) {
			store := namingTestStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var private string
			client := ephemeralNamingMock{run: func(_ context.Context, opts codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
				private = filepath.Dir(opts.Directory)
				content, err := os.ReadFile(opts.InstructionFile)
				if err != nil || string(content) != codex.EphemeralInstructionFileContent {
					t.Fatal("failure path lacks exact private instruction file")
				}
				switch mode {
				case "failure":
					return codex.EphemeralTurnResult{}, codex.ErrEphemeralIsolation
				case "cancel":
					cancel()
					return codex.EphemeralTurnResult{Text: `{"name":"should never commit fallback"}`}, nil
				default:
					return codex.EphemeralTurnResult{Text: strings.Repeat("x", 257)}, nil
				}
			}}
			output, err := modelSessionName(ctx, "raw request", client, store)
			if output != "" || err == nil {
				t.Fatalf("output=%q err=%v", output, err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch state retained: %v", err)
			}
		})
	}
}

func TestSessionNamingUtilityUnsafePrivateStateNeverCallsClient(t *testing.T) {
	for _, mode := range []string{"workspace state", "symlink", "open directory", "invalid utf8"} {
		t.Run(mode, func(t *testing.T) {
			store := namingTestStore(t)
			raw := "raw request"
			switch mode {
			case "workspace state":
				store.directory = filepath.Join(store.workspace, "state")
			case "symlink":
				if store.ensureDirectory() != nil {
					t.Fatal("prepare state")
				}
				if os.Symlink(t.TempDir(), filepath.Join(store.directory, "session-naming")) != nil {
					t.Fatal("symlink")
				}
			case "open directory":
				if store.ensureDirectory() != nil {
					t.Fatal("prepare state")
				}
				if os.Mkdir(filepath.Join(store.directory, "session-naming"), 0755) != nil {
					t.Fatal("mkdir")
				}
			case "invalid utf8":
				raw = string([]byte{255})
			}
			client := ephemeralNamingMock{run: func(context.Context, codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
				t.Fatal("unsafe client call")
				return codex.EphemeralTurnResult{}, nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := modelSessionName(ctx, raw, client, store)
			if !errors.Is(err, codex.ErrEphemeralIsolation) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSessionNamingUtilityErrorUsesExistingFallback(t *testing.T) {
	server := newTestServer(t)
	defer server.Close()
	store := namingTestStore(t)
	client := ephemeralNamingMock{run: func(context.Context, codex.EphemeralTurnOptions) (codex.EphemeralTurnResult, error) {
		return codex.EphemeralTurnResult{}, codex.ErrEphemeralIsolation
	}}
	server.config.SessionNamer = func(ctx context.Context, input string) (string, error) {
		return modelSessionName(ctx, input, client, store)
	}
	name, outcome, err := server.namePreparation(context.Background(), "Fix runtime session names")
	if err != nil || name != "fix-runtime-session-names" || outcome != "utility_error" {
		t.Fatalf("name=%q outcome=%q err=%v", name, outcome, err)
	}
}

func TestSessionNamingUtilityNormalConstructionAndInjectedBoundary(t *testing.T) {
	original := newTestServer(t)
	defer original.Close()
	if original.config.SessionNamer == nil {
		t.Fatal("normal server did not install naming adapter")
	}
	config := original.config
	config.CodexSocket = ""
	config.SessionNamer = nil
	fallback, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Close()
	if fallback.config.SessionNamer != nil {
		t.Fatal("missing trusted socket acquired a helper")
	}
	config.SessionNamer = func(context.Context, string) (string, error) { return `{"name":"injected test naming boundary"}`, nil }
	injected, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer injected.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := injected.config.SessionNamer(ctx, "raw input")
	if err != nil || output != `{"name":"injected test naming boundary"}` {
		t.Fatal("injected seam replaced")
	}
}
