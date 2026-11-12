package codexproviders

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadModelsDiagnostic(t *testing.T) {
	for _, item := range []struct {
		name, script, outcome, message string
		exit                           int
	}{
		{"start", "", "start", "selected provider model catalog is unavailable", -1},
		{"exit", "exit 23", "exit", "selected provider model catalog is unavailable", 23},
		{"signal", "kill -TERM $$", "exit", "selected provider model catalog is unavailable", -1},
		{"invalid", "printf private-output-canary", "invalid", "selected provider model catalog is invalid", 0},
		{"limit", "trap '' PIPE\nprintf '%1048577s' x\nexit 0", "output_limit", "selected provider model catalog is unavailable", 0},
		{"deadline", "sleep 5", "deadline", "selected provider model catalog is unavailable", -1},
		{"canceled", "sleep 5", "canceled", "selected provider model catalog is unavailable", -1},
	} {
		t.Run(item.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "private-path-canary")
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			if item.script != "" {
				bash, err := exec.LookPath("bash")
				if err != nil {
					t.Fatal(err)
				}
				body := "#!" + bash + "\nprintf private-stderr-canary >&2\n" + item.script + "\n"
				if err := os.WriteFile(filepath.Join(root, "bin/workspace-host"), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if item.name == "deadline" {
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			} else if item.name == "canceled" {
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			_, err := ReadModels(ctx, root, root, "workspace-canary", "provider-canary")
			var diagnostic *ModelsReadError
			if !errors.As(err, &diagnostic) || err.Error() != item.message {
				t.Fatalf("wrong public error: %v", err)
			}
			outcome, code := diagnostic.Diagnostic()
			if outcome != item.outcome || code != item.exit {
				t.Fatalf("diagnostic %s/%d, want %s/%d", outcome, code, item.outcome, item.exit)
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatal("private underlying error escaped")
			}
			cancel() // A later context state must not rewrite an actual process result.
			if later, _ := diagnostic.Diagnostic(); later != item.outcome {
				t.Fatal("outcome changed after context cancellation")
			}
		})
	}
	t.Run("identity", func(t *testing.T) {
		_, err := ReadModels(context.Background(), "relative-private-canary", "/tmp", "workspace", "pool")
		var diagnostic *ModelsReadError
		if !errors.As(err, &diagnostic) || err.Error() != "provider model catalog has an invalid identity" {
			t.Fatal(err)
		}
		if outcome, code := diagnostic.Diagnostic(); outcome != "identity" || code != -1 {
			t.Fatal(outcome, code)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		if outcome, code := modelProcessDiagnostic(errors.New("private-error-canary"), &exec.Cmd{}, false); outcome != "unknown" || code != -1 {
			t.Fatal(outcome, code)
		}
	})
	for _, output := range []string{"[]", "null"} {
		t.Run("valid_"+output, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			bash, err := exec.LookPath("bash")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "bin/workspace-host"), []byte("#!"+bash+"\nprintf '%s' '"+output+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			models, err := ReadModels(context.Background(), root, root, "workspace", "pool")
			if err != nil || len(models) != 0 || (output == "null" && models != nil) {
				t.Fatalf("catalog semantics changed: %v %v", models, err)
			}
		})
	}
}
