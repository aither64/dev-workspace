package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
)

const proofThreadID = "11111111-1111-7111-8111-111111111111"
const proofProjectID = "22222222-2222-7222-8222-222222222222"

type archiveProofReader struct {
	metadata codex.ThreadMetadata
	err      error
	reads    int
	onRead   func(int)
}

func (reader *archiveProofReader) ReadThreadMetadata(_ context.Context, id string, excludeTurns bool) (codex.ThreadMetadata, error) {
	reader.reads++
	if reader.onRead != nil {
		reader.onRead(reader.reads)
	}
	if id != proofThreadID || excludeTurns {
		return codex.ThreadMetadata{}, errors.New("proof used the wrong metadata request")
	}
	return reader.metadata, reader.err
}

func archiveProofFixture(t *testing.T) (ArchivedThreadIdentity, *archiveProofReader, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "nondefault-codex-home")
	path := filepath.Join(home, "archived_sessions", "rollout-2026-09-29T10-00-00-"+proofThreadID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(t.TempDir(), "work", "2026-09-29-example")
	record, err := json.Marshal(map[string]any{
		"type": "session_meta", "payload": map[string]any{"id": proofThreadID, "cwd": cwd},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(record, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := ArchivedThreadIdentity{ThreadID: proofThreadID, Cwd: cwd, ProjectID: proofProjectID, CodexHome: home}
	reader := &archiveProofReader{metadata: codex.ThreadMetadata{
		ID: proofThreadID, Cwd: cwd, ProjectID: pointer(proofProjectID), Path: &path,
	}}
	return identity, reader, path
}

func pointer(value string) *string { return &value }

func TestProveArchivedThreadUsesExactMetadataAndBoundedHeader(t *testing.T) {
	identity, reader, path := archiveProofFixture(t)
	state, err := ProveArchivedThread(context.Background(), reader, identity)
	if err != nil || state != ArchiveArchived || reader.reads != 2 {
		t.Fatalf("exact archived proof = %v, %v, reads=%d", state, err, reader.reads)
	}
	identity.ProjectID = ""
	reader.metadata.ProjectID = nil
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofThreadID+`"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err = ProveArchivedThread(context.Background(), reader, identity)
	if err != nil || state != ArchiveArchived {
		t.Fatalf("retained legacy identity = %v, %v", state, err)
	}
}

func TestProveArchivedRootThreadRejectsForeignSource(t *testing.T) {
	identity, reader, _ := archiveProofFixture(t)
	identity.SourceKind = threadSourceKind
	reader.metadata.Source = "cli"
	if state, err := ProveArchivedThread(context.Background(), reader, identity); err == nil || state != ArchiveUnknown || reader.reads != 1 {
		t.Fatalf("foreign root source passed: %v, %v, reads=%d", state, err, reader.reads)
	}
}

func TestProveArchivedThreadKeepsNonarchivedAndUnknownDistinct(t *testing.T) {
	identity, reader, path := archiveProofFixture(t)
	reader.metadata.Path = nil
	if state, err := ProveArchivedThread(context.Background(), reader, identity); err != nil || state != ArchiveFresh {
		t.Fatalf("fresh = %v, %v", state, err)
	}
	active := filepath.Join(identity.CodexHome, "sessions", filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatal(err)
	}
	reader.metadata.Path = &active
	if state, err := ProveArchivedThread(context.Background(), reader, identity); err != nil || state != ArchiveActive {
		t.Fatalf("active = %v, %v", state, err)
	}
	reader.err = &codex.ThreadNotFoundError{ThreadID: identity.ThreadID}
	if state, err := ProveArchivedThread(context.Background(), reader, identity); err == nil || state != ArchiveUnknown {
		t.Fatalf("not-found became archive evidence: %v, %v", state, err)
	}
	reader.err = errors.New("thread is archived")
	if state, err := ProveArchivedThread(context.Background(), reader, identity); err == nil || state != ArchiveUnknown {
		t.Fatalf("typed/message error became archive evidence: %v, %v", state, err)
	}
}

func TestProveArchivedThreadRejectsConflictingEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, expected *ArchivedThreadIdentity, reader *archiveProofReader, path string)
	}{
		{"wrong returned id", func(_ *testing.T, _ *ArchivedThreadIdentity, reader *archiveProofReader, _ string) {
			reader.metadata.ID = proofProjectID
		}},
		{"wrong cwd", func(_ *testing.T, _ *ArchivedThreadIdentity, reader *archiveProofReader, _ string) {
			reader.metadata.Cwd += "-other"
		}},
		{"null project", func(_ *testing.T, _ *ArchivedThreadIdentity, reader *archiveProofReader, _ string) {
			reader.metadata.ProjectID = nil
		}},
		{"wrong project", func(_ *testing.T, _ *ArchivedThreadIdentity, reader *archiveProofReader, _ string) {
			reader.metadata.ProjectID = pointer(proofThreadID)
		}},
		{"wrong home", func(_ *testing.T, expected *ArchivedThreadIdentity, _ *archiveProofReader, _ string) {
			expected.CodexHome = filepath.Dir(expected.CodexHome)
		}},
		{"sibling archive", func(_ *testing.T, expected *ArchivedThreadIdentity, reader *archiveProofReader, path string) {
			other := filepath.Join(expected.CodexHome, "archived_sessions_backup", filepath.Base(path))
			reader.metadata.Path = &other
		}},
		{"wrong filename", func(_ *testing.T, expected *ArchivedThreadIdentity, reader *archiveProofReader, _ string) {
			other := filepath.Join(expected.CodexHome, "archived_sessions", "rollout-2026-09-29T10-00-00-"+proofProjectID+".jsonl")
			reader.metadata.Path = &other
		}},
		{"missing rollout", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink rollout", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			other := path + ".real"
			if err := os.Rename(path, other); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory rollout", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized header", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("x", archiveHeaderLimit+1)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt header", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.WriteFile(path, []byte("{\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong header id", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofProjectID+`"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong header cwd", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofThreadID+`","cwd":"/other"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate header id", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofProjectID+`","id":"`+proofThreadID+`"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"null header cwd", func(t *testing.T, _ *ArchivedThreadIdentity, _ *archiveProofReader, path string) {
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofThreadID+`","cwd":null}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity, reader, path := archiveProofFixture(t)
			tc.change(t, &identity, reader, path)
			if state, err := ProveArchivedThread(context.Background(), reader, identity); err == nil || state != ArchiveUnknown {
				t.Fatalf("conflicting evidence passed: %v, %v", state, err)
			}
		})
	}
}

func TestProveArchivedThreadRejectsMetadataAndFileRaces(t *testing.T) {
	t.Run("metadata changes", func(t *testing.T) {
		identity, reader, _ := archiveProofFixture(t)
		reader.onRead = func(n int) {
			if n == 2 {
				reader.metadata.Cwd = "/other"
			}
		}
		if state, err := ProveArchivedThread(context.Background(), reader, identity); err == nil || state != ArchiveUnknown {
			t.Fatalf("changed metadata passed: %v, %v", state, err)
		}
	})
	t.Run("path replacement", func(t *testing.T) {
		identity, reader, path := archiveProofFixture(t)
		reader.onRead = func(n int) {
			if n != 2 {
				return
			}
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+proofThreadID+`"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if state, err := ProveArchivedThread(context.Background(), reader, identity); err == nil || state != ArchiveUnknown {
			t.Fatalf("replaced file passed: %v, %v", state, err)
		}
	})
}
