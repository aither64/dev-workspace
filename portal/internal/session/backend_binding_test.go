package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"golang.org/x/sys/unix"
)

func backendBindingFixture() BackendBinding {
	return BackendBinding{
		Schema: 1, Backend: agentbackend.Pi, BackendContract: 1,
		Workspace: "/srv/workspace", Slug: "example", SessionIncarnation: strings.Repeat("a", 64),
		CreationReceiptID: "original-create", Revision: 1, State: "ready",
		Conversations: []BackendConversation{
			{Address: "lead", Incarnation: strings.Repeat("b", 64), State: "ready", NativeID: "pi-native-lead"},
			{Address: "implementer2", Incarnation: strings.Repeat("c", 64), State: "ready", NativeID: "pi-native-member"},
		},
	}
}

func backendJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func backendWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func backendBindingPath(root string) string {
	digest := sha256.Sum256([]byte("/srv/workspace"))
	return filepath.Join(root, "session-backends", hex.EncodeToString(digest[:]), "example.json")
}

func mutateBackendJSON(t *testing.T, data []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	mutate(object)
	return backendJSON(t, object)
}

func TestBackendBindingReadsAreStrictAndDoNotPublishState(t *testing.T) {
	root := t.TempDir()
	store := BackendBindingStore{Root: root, Workspace: "/srv/workspace"}
	if _, err := store.Load("example"); !errors.Is(err, agentbackend.ErrNotBound) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing binding: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read created state: %v, %v", entries, err)
	}
	path := backendBindingPath(root)
	data := append(backendJSON(t, backendBindingFixture()), '\n')
	backendWrite(t, path, data)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		loaded, err := store.Load("example")
		if err != nil {
			t.Fatal(err)
		}
		member, err := loaded.Resolve("implementer2")
		if err != nil || member.Backend != agentbackend.Pi || member.NativeID != "pi-native-member" || member.Session.Incarnation != strings.Repeat("a", 64) {
			t.Fatalf("resolved wrong member: %#v, %v", member, err)
		}
	}
	after, err := os.Stat(path)
	if err != nil || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		t.Fatalf("read changed file metadata: %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, unchanged) {
		t.Fatalf("read changed bytes: %v", err)
	}
	entries, err = os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "example.json" {
		t.Fatalf("read published extra state: %v, %v", entries, err)
	}
}

func TestBackendBindingRejectsInvalidJSONAndIdentity(t *testing.T) {
	valid := backendJSON(t, backendBindingFixture())
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"duplicate", bytes.Replace(valid, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1), nil},
		{"escaped-duplicate", bytes.Replace(valid, []byte(`"schema":1`), []byte(`"schema":1,"\u0073chema":1`), 1), nil},
		{"nested-duplicate", bytes.Replace(valid, []byte(`"address":"lead"`), []byte(`"address":"lead","address":"lead"`), 1), nil},
		{"nested-unknown-case", bytes.Replace(valid, []byte(`"address":"lead"`), []byte(`"address":"lead","ADDRESS":"lead"`), 1), nil},
		{"trailing", append(append([]byte{}, valid...), []byte(` {}`)...), nil},
		{"empty-native", bytes.Replace(valid, []byte(`"native_id":"pi-native-lead"`), []byte(`"native_id":""`), 1), nil},
		{"invalid-utf8", bytes.Replace(valid, []byte("original-create"), []byte{0xff}, 1), nil},
		{"fractional-revision", bytes.Replace(valid, []byte(`"revision":1`), []byte(`"revision":1.5`), 1), nil},
		{"float-schema", bytes.Replace(valid, []byte(`"schema":1`), []byte(`"schema":1.0`), 1), nil},
		{"revision-overflow", bytes.Replace(valid, []byte(`"revision":1`), []byte(`"revision":18446744073709551616`), 1), nil},
		{"top-array", []byte(`[]`), nil},
	}
	for _, field := range []string{"schema", "backend", "backend_contract", "workspace", "slug", "session_incarnation", "creation_receipt_id", "revision", "state", "conversations"} {
		for _, operation := range []string{"missing", "null"} {
			tests = append(tests, struct {
				name string
				data []byte
				want error
			}{operation + "-" + field, mutateBackendJSON(t, valid, func(object map[string]any) {
				if operation == "missing" {
					delete(object, field)
				} else {
					object[field] = nil
				}
			}), nil})
		}
	}
	for _, tc := range []struct {
		name, field string
		value       any
		want        error
	}{
		{"unknown", "extra", true, nil}, {"unknown-case", "SCHEMA", 1, nil},
		{"schema", "schema", 2, agentbackend.ErrUnsupportedBinding},
		{"backend", "backend", "codex", agentbackend.ErrUnsupportedBinding},
		{"contract", "backend_contract", 2, agentbackend.ErrUnsupportedBinding},
		{"workspace", "workspace", "/srv/other", nil}, {"slug", "slug", "other", nil},
		{"incarnation", "session_incarnation", strings.Repeat("A", 64), nil},
		{"receipt-limit", "creation_receipt_id", strings.Repeat("é", 2049), nil},
		{"empty-receipt", "creation_receipt_id", "", nil},
		{"zero-revision", "revision", 0, nil}, {"negative-revision", "revision", -1, nil},
		{"state", "state", "complete", nil}, {"empty-members", "conversations", []any{}, nil},
	} {
		tests = append(tests, struct {
			name string
			data []byte
			want error
		}{tc.name, mutateBackendJSON(t, valid, func(object map[string]any) { object[tc.field] = tc.value }), tc.want})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			backendWrite(t, backendBindingPath(root), tc.data)
			loaded, err := (BackendBindingStore{root, "/srv/workspace"}).Load("example")
			if err == nil || errors.Is(err, agentbackend.ErrNotBound) || loaded.Backend != "" {
				t.Fatalf("invalid record became usable/absent: %#v, %v", loaded, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBackendBindingConversationStatesAndUniqueness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*BackendBinding)
	}{
		{"duplicate-lead", func(b *BackendBinding) { b.Conversations[1].Address = "lead" }},
		{"no-lead", func(b *BackendBinding) { b.Conversations[0].Address = "reviewer0" }},
		{"duplicate-native", func(b *BackendBinding) { b.Conversations[1].NativeID = b.Conversations[0].NativeID }},
		{"missing-ready-native", func(b *BackendBinding) { b.Conversations[0].NativeID = "" }},
		{"invalid-member", func(b *BackendBinding) { b.Conversations[1].Address = "../lead" }},
		{"invalid-incarnation", func(b *BackendBinding) { b.Conversations[1].Incarnation = "old" }},
		{"unready-lead", func(b *BackendBinding) { b.Conversations[0].State = "creating" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := backendBindingFixture()
			tc.mutate(&binding)
			if err := binding.Validate(binding.Workspace, binding.Slug); err == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
	for _, nativeID := range []string{"", "allocated-but-not-ready"} {
		binding := backendBindingFixture()
		binding.Conversations[1].State = "creating"
		binding.Conversations[1].NativeID = nativeID
		root := t.TempDir()
		backendWrite(t, backendBindingPath(root), backendJSON(t, binding))
		loaded, err := (BackendBindingStore{root, binding.Workspace}).Load(binding.Slug)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loaded.Resolve("implementer2"); !errors.Is(err, agentbackend.ErrConversationNotReady) {
			t.Fatalf("creating member dispatchable: %v", err)
		}
		if _, err := loaded.Resolve("lead"); err != nil {
			t.Fatalf("ready lead refused: %v", err)
		}
		binding.State = "creating"
		if _, err := binding.Resolve("lead"); !errors.Is(err, agentbackend.ErrConversationNotReady) {
			t.Fatalf("creating binding dispatchable: %v", err)
		}
	}
	for _, nativeJSON := range []string{`""`, `null`} {
		binding := backendBindingFixture()
		binding.Conversations[1].State = "creating"
		data := bytes.Replace(backendJSON(t, binding), []byte(`"native_id":"pi-native-member"`), []byte(`"native_id":`+nativeJSON), 1)
		root := t.TempDir()
		backendWrite(t, backendBindingPath(root), data)
		if _, err := (BackendBindingStore{root, binding.Workspace}).Load(binding.Slug); err == nil {
			t.Fatalf("creating member accepted explicit native_id %s", nativeJSON)
		}
	}
	binding := backendBindingFixture()
	if _, err := binding.Resolve("reviewer0"); !errors.Is(err, agentbackend.ErrBindingConflict) {
		t.Fatalf("unbound member resolved: %v", err)
	}
}

func TestBackendRecordRejectsUnsafeFilesAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(string) error
	}{
		{"public-file", func(path string) error { return os.Chmod(path, 0644) }},
		{"public-directory", func(path string) error { return os.Chmod(filepath.Dir(path), 0755) }},
		{"symlink", func(path string) error {
			if err := os.Rename(path, path+".target"); err != nil {
				return err
			}
			return os.Symlink(path+".target", path)
		}},
		{"fifo", func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return unix.Mkfifo(path, 0600)
		}},
		{"size", func(path string) error {
			return os.WriteFile(path, bytes.Repeat([]byte(" "), authorityMaxSize+1), 0600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := backendBindingPath(root)
			backendWrite(t, path, backendJSON(t, backendBindingFixture()))
			if err := tc.set(path); err != nil {
				t.Fatal(err)
			}
			if _, err := (BackendBindingStore{root, "/srv/workspace"}).Load("example"); err == nil || errors.Is(err, agentbackend.ErrNotBound) {
				t.Fatalf("invalid file accepted/treated as absent: %v", err)
			}
		})
	}
	for _, store := range []BackendBindingStore{{Root: "relative", Workspace: "/srv/workspace"}, {Root: t.TempDir(), Workspace: "/srv/../workspace"}} {
		if _, err := store.Load("example"); err == nil {
			t.Fatal("noncanonical scope accepted")
		}
	}
	if _, err := (BackendBindingStore{t.TempDir(), "/srv/workspace"}).Load("../other"); err == nil {
		t.Fatal("path-valued slug accepted")
	}
}
