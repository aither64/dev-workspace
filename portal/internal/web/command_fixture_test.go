package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Share the ordinary command launcher without intercepting command behavior.
func fixtureDevSessionCommand(t *testing.T, _ string, command string) string {
	t.Helper()
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nexec " + quote(command) + " \"$@\"\n"
	path := filepath.Join(t.TempDir(), "dev-session-command")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
