package agentteams

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerificationWatcherInstructionsUsesValidatedNativeIdentity(t *testing.T) {
	catalog := catalogFixture(t)
	instructions := "Observe exact input.\nKeep saved policy: \"yes\"."
	v := &catalog.NativeAgentConfigs.Utilities[0]
	v.Identity.BehaviorDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(instructions)))
	v.Name = expectedUtilityName(catalog.CatalogDigest, *v)
	root := t.TempDir()
	path := filepath.Join(root, v.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	name, _ := json.Marshal(v.Name)
	body, _ := json.Marshal(instructions)
	valid := fmt.Sprintf("name = %s\ndescription = \"Watcher\"\ndeveloper_instructions = %s\nsandbox_mode = \"danger-full-access\"\napproval_policy = \"never\"\n", name, body)
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", valid, true},
		{"wrong digest", strings.Replace(valid, "Observe exact", "Observe other", 1), false},
		{"duplicate", valid + "developer_instructions = \"duplicate\"\n", false},
		{"wrong policy", strings.Replace(valid, "danger-full-access", "read-only", 1), false},
		{"non-generated toml", strings.Replace(valid, string(body), "'''arbitrary TOML'''", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0644); err != nil {
				t.Fatal(err)
			}
			got, err := VerificationWatcherInstructions(root, catalog)
			if tc.ok && (err != nil || got != instructions) || !tc.ok && err == nil {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
