package session

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureSlug = "2026-09-03-example"

func TestManifestFixtureLoadsAndMatchesDirectory(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	summary, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Slug != fixtureSlug || len(summary.Artifacts) != 1 || !summary.Closed {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestManifestAcceptsASeparateForkSource(t *testing.T) {
	var manifest Manifest
	data := []byte("schema: 1\nslug: 2026-09-04-fork\nforked_from: 2026-09-03-source\n")
	if err := decodeManifest(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate("2026-09-04-fork"); err != nil {
		t.Fatal(err)
	}
	if manifest.ForkedFrom != "2026-09-03-source" {
		t.Fatalf("fork source = %q", manifest.ForkedFrom)
	}
	manifest.ForkedFrom = manifest.Slug
	if err := manifest.Validate(manifest.Slug); err == nil {
		t.Fatal("self fork was accepted")
	}
}

func TestListSortsBySessionDateBeforeLastUpdate(t *testing.T) {
	workspace := t.TempDir()
	writeSession := func(root, lifecycle, slug string, updatedAt time.Time) {
		directory := filepath.Join(workspace, root, slug)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		paths := []string{
			filepath.Join(directory, ManifestName),
			filepath.Join(directory, "state.md"),
			filepath.Join(directory, "plan.md"),
		}
		manifest := "schema: 1\nslug: " + slug + "\n"
		if root == "archive" {
			manifest += "finalized_at: \"" + updatedAt.Format(time.RFC3339) + "\"\n"
		}
		if err := os.WriteFile(paths[0], []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		writeTrackingFiles(t, directory, lifecycle)
		for _, path := range paths {
			if err := os.Chtimes(path, updatedAt, updatedAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	sharedTime := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	writeSession("work", "active", "2026-09-03-edited-later", sharedTime.Add(6*time.Hour))
	writeSession("work", "active", "2026-09-04-zulu", sharedTime)
	writeSession("work", "active", "2026-09-04-alpha", sharedTime)
	writeSession("archive", "complete", "2026-09-05-newest", sharedTime)
	writeSession("archive", "complete", "2026-09-04-arch-zulu", sharedTime)
	writeSession("archive", "complete", "2026-09-04-arch-alpha", sharedTime)

	summaries, err := List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		got = append(got, summary.Slug)
	}
	want := []string{
		"2026-09-04-alpha", "2026-09-04-zulu", "2026-09-03-edited-later",
		"2026-09-05-newest", "2026-09-04-arch-alpha", "2026-09-04-arch-zulu",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("session order = %v, want %v", got, want)
	}
}

func TestSharedValidManifestFixturesAreAccepted(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("..", "..", "..", "test", "fixtures", "portal-manifest-valid*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 2 {
		t.Fatal("schema 1 and schema 2 valid manifest fixtures are required")
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			data, readErr := os.ReadFile(fixture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var manifest Manifest
			if err := decodeManifest(data, &manifest); err != nil {
				t.Fatalf("valid shared fixture failed decoding: %v", err)
			}
			if err := manifest.Validate(fixtureSlug); err != nil {
				t.Fatalf("valid shared fixture failed validation: %v", err)
			}
		})
	}
}

func TestInitialGoalAttemptMarkerIsLimitedToInFlightSchema(t *testing.T) {
	attempted := true
	for _, testCase := range []struct {
		name     string
		manifest Manifest
		valid    bool
	}{
		{
			name: "in-flight schema",
			manifest: Manifest{
				Schema: 2, Slug: "example",
				Creation: Creation{State: "creating", InitialGoalAttempted: &attempted},
			},
			valid: true,
		},
		{
			name: "marker in stable schema",
			manifest: Manifest{
				Schema: 1, Slug: "example",
				Creation: Creation{State: "creating", InitialGoalAttempted: &attempted},
			},
		},
		{
			name: "ready in-flight schema",
			manifest: Manifest{
				Schema: 2, Slug: "example",
				Creation: Creation{State: "ready", InitialGoalAttempted: &attempted},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.manifest.Validate("example")
			if testCase.valid && err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
			if !testCase.valid && err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestArchiveChronologyUsesFinalizedTimestamp(t *testing.T) {
	workspace := fixtureWorkspace(t, "archive")
	summary, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := time.Parse(time.RFC3339, "2026-09-03T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !summary.UpdatedAt.Equal(expected) {
		t.Fatalf("archive timestamp = %s, expected %s", summary.UpdatedAt, expected)
	}
}

func TestSharedInvalidManifestFixturesAreRejected(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("..", "..", "..", "test", "fixtures", "portal-manifest-invalid-*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no invalid manifest fixtures found")
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			workspace := t.TempDir()
			destination := filepath.Join(workspace, "work", fixtureSlug, ManifestName)
			data, readErr := os.ReadFile(fixture)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Find(workspace, fixtureSlug); err == nil {
				t.Fatal("invalid shared fixture was accepted")
			}
		})
	}
}

func TestSharedLifecycleFixtures(t *testing.T) {
	for _, pattern := range []struct {
		glob  string
		valid bool
	}{
		{"lifecycle-valid-*.md", true},
		{"lifecycle-invalid-*.md", false},
	} {
		fixtures, err := filepath.Glob(filepath.Join("..", "..", "..", "test", "fixtures", pattern.glob))
		if err != nil {
			t.Fatal(err)
		}
		if len(fixtures) == 0 {
			t.Fatalf("no lifecycle fixtures for %s", pattern.glob)
		}
		for _, fixture := range fixtures {
			t.Run(filepath.Base(fixture), func(t *testing.T) {
				workspace := t.TempDir()
				directory := filepath.Join(workspace, "work", fixtureSlug)
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "state.md"), data, 0o644); err != nil {
					t.Fatal(err)
				}
				_, _, err = loadLifecycle(workspace, "work", fixtureSlug)
				if pattern.valid && err != nil {
					t.Fatalf("valid lifecycle rejected: %v", err)
				}
				if !pattern.valid && err == nil {
					t.Fatal("invalid lifecycle accepted")
				}
			})
		}
	}
}

func TestLifecycleRejectsInvalidUTF8AndOversizedInput(t *testing.T) {
	for _, testCase := range []struct {
		name string
		data []byte
	}{
		{"invalid-utf8", append([]byte("---\nlifecycle: active\n---\n"), 0xff)},
		{"oversized", []byte(strings.Repeat("x", manifestMaxSize+1))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, "work", fixtureSlug)
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "state.md"), testCase.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadLifecycle(workspace, "work", fixtureSlug); err == nil {
				t.Fatal("invalid lifecycle input accepted")
			}
		})
	}
}

func TestManifestSlugMustMatchDirectory(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	path := filepath.Join(workspace, "work", fixtureSlug, ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), fixtureSlug, "2026-09-03-other", 1))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Find(workspace, fixtureSlug); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected identity error, got %v", err)
	}
}

func TestWorkspaceManifestNeverGrantsRuntimeInteractivity(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ManifestName)
	creating := "schema: 2\nslug: example\ncodex:\n  thread_id: thread-1\n  socket_path: /run/codex.sock\n  client_version: 0.152.1\ncreation:\n  state: creating\n  initial_goal_sent: false\n  initial_goal_attempted: true\n  goal_sha256: " + strings.Repeat("a", 64) + "\nrepositories: []\nartifacts: []\n"
	if err := os.WriteFile(path, []byte(creating), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTrackingFiles(t, directory, "active")
	summary, err := Find(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Interactive {
		t.Fatal("creating session is interactive")
	}
	ready := strings.Replace(creating, "state: creating", "state: ready", 1)
	ready = strings.Replace(ready, "initial_goal_sent: false", "initial_goal_sent: true", 1)
	ready = strings.Replace(ready, "schema: 2", "schema: 1", 1)
	ready = strings.Replace(ready, "  initial_goal_attempted: true\n", "", 1)
	if err := os.WriteFile(path, []byte(ready), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err = Find(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Interactive {
		t.Fatal("writable workspace metadata granted runtime interactivity")
	}
}

func TestTerminalLifecycleMakesWorkSessionReadOnly(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "work", "example")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schema: 1\nslug: example\ncodex:\n  thread_id: thread-1\ncreation:\n  state: ready\nrepositories: []\n"
	if err := os.WriteFile(filepath.Join(directory, ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTrackingFiles(t, directory, "complete")
	summary, err := Find(workspace, "example")
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Closed || summary.Interactive || summary.Lifecycle != "complete" {
		t.Fatalf("terminal session phase = %#v", summary)
	}
}

func TestArchiveRequiresTerminalLifecycleAndFinalization(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		lifecycle string
		finalized bool
	}{
		{"active", "active", true},
		{"not-finalized", "complete", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, "archive", "example")
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			manifest := "schema: 1\nslug: example\nrepositories: []\n"
			if testCase.finalized {
				manifest += "finalized_at: \"2026-09-03T12:00:00Z\"\n"
			}
			if err := os.WriteFile(filepath.Join(directory, ManifestName), []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			writeTrackingFiles(t, directory, testCase.lifecycle)
			if _, err := Find(workspace, "example"); err == nil {
				t.Fatal("invalid archive phase was accepted")
			}
		})
	}
}

func TestActiveLifecycleRejectsFinalizationMetadata(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	directory := filepath.Join(workspace, "work", fixtureSlug)
	writeTrackingFiles(t, directory, "active")
	if _, err := Find(workspace, fixtureSlug); err == nil {
		t.Fatal("active lifecycle with finalization metadata was accepted")
	}
}

func TestDuplicateActiveAndArchivedSessionIsRejected(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	copyFixture(t, filepath.Join(workspace, "archive", fixtureSlug, ManifestName))
	if _, err := Find(workspace, fixtureSlug); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestSessionAndManifestSymlinksAreRejected(t *testing.T) {
	for _, target := range []string{"session", "manifest"} {
		t.Run(target, func(t *testing.T) {
			workspace := t.TempDir()
			outside := t.TempDir()
			if err := os.Mkdir(filepath.Join(workspace, "work"), 0o755); err != nil {
				t.Fatal(err)
			}
			if target == "session" {
				copyFixture(t, filepath.Join(outside, ManifestName))
				if err := os.Symlink(outside, filepath.Join(workspace, "work", fixtureSlug)); err != nil {
					t.Fatal(err)
				}
			} else {
				directory := filepath.Join(workspace, "work", fixtureSlug)
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				copyFixture(t, filepath.Join(outside, ManifestName))
				if err := os.Symlink(filepath.Join(outside, ManifestName), filepath.Join(directory, ManifestName)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Find(workspace, fixtureSlug); err == nil {
				t.Fatal("expected symlink to be rejected")
			}
		})
	}
}

func TestArtifactOpenIsConfinedAndRaceSafe(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	directory := filepath.Join(workspace, "work", fixtureSlug)
	if err := os.WriteFile(filepath.Join(directory, "report.md"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := OpenArtifact(summary, "report.md", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(filepath.Join(directory, "report.md"), filepath.Join(directory, "old.md")); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(directory, "report.md")); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "safe" {
		t.Fatalf("opened descriptor changed to %q", data)
	}
}

func TestArtifactRejectsFinalAndIntermediateSymlinks(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	directory := filepath.Join(workspace, "work", fixtureSlug)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(directory, "report.md")); err != nil {
		t.Fatal(err)
	}
	summary, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenArtifact(summary, "report.md", 1024); err == nil {
		t.Fatal("expected final symlink rejection")
	}
	if err := os.Remove(filepath.Join(directory, "report.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "linked")); err != nil {
		t.Fatal(err)
	}
	summary.Artifacts = append(summary.Artifacts, Artifact{Label: "Nested", Path: "linked/secret.md"})
	if _, _, err := OpenArtifact(summary, "linked/secret.md", 1024); err == nil {
		t.Fatal("expected intermediate symlink rejection")
	}
}

func TestWorkspaceRootSymlinkIsRejected(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "actual")
	copyFixture(t, filepath.Join(workspace, "work", fixtureSlug, ManifestName))
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Find(alias, fixtureSlug); err == nil {
		t.Fatal("expected workspace root symlink rejection")
	}
}

func TestOversizedArtifactIsRejectedBeforeReading(t *testing.T) {
	workspace := fixtureWorkspace(t, "work")
	directory := filepath.Join(workspace, "work", fixtureSlug)
	if err := os.WriteFile(filepath.Join(directory, "report.md"), []byte(strings.Repeat("x", 1025)), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := Find(workspace, fixtureSlug)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenArtifact(summary, "report.md", 1024); err == nil {
		t.Fatal("expected oversized artifact rejection")
	}
}

func fixtureWorkspace(t *testing.T, root string) string {
	t.Helper()
	workspace := t.TempDir()
	copyFixture(t, filepath.Join(workspace, root, fixtureSlug, ManifestName))
	return workspace
}

func copyFixture(t *testing.T, destination string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "portal-manifest-valid.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTrackingFiles(t, filepath.Dir(destination), "complete")
}

func writeTrackingFiles(t *testing.T, directory, lifecycle string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(directory, "state.md"),
		[]byte("---\nlifecycle: "+lifecycle+"\n---\n\n# State\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("# Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
