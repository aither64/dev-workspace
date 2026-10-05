package workspacecodex

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aither64/codex-web/codex"
)

type threadlessTestClient struct {
	options                []codex.ThreadListOptions
	binding                string
	lookupError            error
	discoveryError         error
	archivedResidue        bool
	cursor                 bool
	lookups                []string
	savedChecks            int
	afterSaved             func()
	loadedIDs              []string
	loadedError            error
	metadata               map[string]codex.ThreadMetadata
	metadataError          error
	metadataReads          []string
	deprecatedExcludeTurns bool
	saved                  []codex.ThreadMetadata
}

func (client *threadlessTestClient) ThreadOperationAttempt(cwd string) (string, error) {
	client.lookups = append(client.lookups, cwd)
	return client.binding, client.lookupError
}
func (client *threadlessTestClient) RequireSavedConversationAbsence(context.Context, string) error {
	client.savedChecks++
	if client.afterSaved != nil {
		client.afterSaved()
	}
	if client.discoveryError != nil {
		return client.discoveryError
	}
	if client.archivedResidue || client.cursor {
		return errors.New("saved scope not proved")
	}
	return nil
}
func (client *threadlessTestClient) ListThreads(_ context.Context, options codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error) {
	client.options = append(client.options, options)
	if client.discoveryError != nil {
		return nil, nil, client.discoveryError
	}
	if client.archivedResidue && *options.Archived {
		return []codex.ThreadMetadata{{ID: "unknown-exec", Cwd: options.Cwd, Source: "exec"}}, nil, nil
	}
	if client.cursor {
		next := "more"
		return nil, &next, nil
	}
	return client.saved, nil, nil
}
func (client *threadlessTestClient) LoadedThreadIDs(context.Context) ([]string, error) {
	return client.loadedIDs, client.loadedError
}
func (client *threadlessTestClient) ReadThreadMetadata(_ context.Context, id string, excludeTurns bool) (codex.ThreadMetadata, error) {
	client.metadataReads = append(client.metadataReads, id)
	client.deprecatedExcludeTurns = client.deprecatedExcludeTurns || excludeTurns
	return client.metadata[id], client.metadataError
}
func TestThreadlessObservationRequiresCompleteDiscoveryAndPublicOperationAbsence(t *testing.T) {
	for _, scenario := range []string{"empty", "operation", "lookup failure", "discovery failure", "archived exec", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			client := &threadlessTestClient{}
			switch scenario {
			case "operation":
				client.binding = "retained"
			case "lookup failure":
				client.lookupError = errors.New("opaque failure")
			case "discovery failure":
				client.discoveryError = errors.New("opaque failure")
			case "archived exec":
				client.archivedResidue = true
			case "incomplete":
				client.cursor = true
			}
			err := RequireThreadlessConversations(context.Background(), client, "/workspace/work/example")
			if (err == nil) != (scenario == "empty") {
				t.Fatalf("proof=%v", err)
			}
			if len(client.options) != 0 {
				t.Fatal("threadless proof used unpartitioned saved discovery")
			}
			if scenario == "empty" && (client.savedChecks != 1 || len(client.lookups) != 2) {
				t.Fatalf("proof not bracketed: %#v", client)
			}
		})
	}
}

func TestThreadlessObservationIncludesLoadedOnlyIdentities(t *testing.T) {
	const cwd = "/workspace/work/example"
	for _, scenario := range []string{"same CWD", "other CWD", "loaded failure", "metadata failure", "wrong ID", "missing CWD", "unclean CWD", "duplicate ID"} {
		t.Run(scenario, func(t *testing.T) {
			client := &threadlessTestClient{loadedIDs: []string{"loaded-only"}, metadata: map[string]codex.ThreadMetadata{
				"loaded-only": {ID: "loaded-only", Cwd: cwd, Source: "exec"},
			}}
			metadata := client.metadata["loaded-only"]
			switch scenario {
			case "other CWD":
				metadata.Cwd = "/workspace/work/another"
			case "loaded failure":
				client.loadedError = errors.New("incomplete public pagination")
			case "metadata failure":
				client.metadataError = errors.New("loaded thread disappeared")
			case "wrong ID":
				metadata.ID = "changed"
			case "missing CWD":
				metadata.Cwd = ""
			case "unclean CWD":
				metadata.Cwd = cwd + "/../another"
			case "duplicate ID":
				client.loadedIDs = append(client.loadedIDs, "loaded-only")
			}
			client.metadata["loaded-only"] = metadata
			err := RequireThreadlessConversations(context.Background(), client, cwd)
			if (err == nil) != (scenario == "other CWD") {
				t.Fatalf("proof=%v", err)
			}
			if client.savedChecks != 1 || client.deprecatedExcludeTurns {
				t.Fatalf("discovery changed its bounded read contract: %#v", client)
			}
			if scenario == "other CWD" && (len(client.metadataReads) != 1 || len(client.lookups) != 2) {
				t.Fatalf("unrelated identity not positively checked: %#v", client)
			}
		})
	}
}

func TestArchiveDiscoveryUnionsExactSavedAndLoadedRetainedIdentities(t *testing.T) {
	const cwd = "/workspace/work/example"
	for _, scenario := range []string{"loaded fresh root", "matching overlap", "unknown saved", "unknown loaded", "other CWD", "changed overlap CWD", "changed overlap source", "changed member project", "absent retained", "absent member"} {
		t.Run(scenario, func(t *testing.T) {
			root := codex.ThreadMetadata{ID: "root", Cwd: cwd, Source: "vscode"}
			project := "project"
			member := codex.ThreadMetadata{ID: "member", Cwd: cwd, Source: "vscode", ProjectID: &project}
			client := &threadlessTestClient{loadedIDs: []string{"root"}, metadata: map[string]codex.ThreadMetadata{"root": root}}
			active := map[string]string{"root": ""}
			switch scenario {
			case "matching overlap":
				client.saved = []codex.ThreadMetadata{root}
			case "unknown saved":
				client.saved = []codex.ThreadMetadata{root, {ID: "unknown", Cwd: cwd, Source: "exec"}}
			case "unknown loaded":
				client.loadedIDs = []string{"unknown"}
				client.metadata["unknown"] = codex.ThreadMetadata{ID: "unknown", Cwd: cwd, Source: "exec"}
				client.saved = []codex.ThreadMetadata{root}
			case "other CWD":
				client.loadedIDs = append(client.loadedIDs, "another")
				client.metadata["another"] = codex.ThreadMetadata{ID: "another", Cwd: "/workspace/work/another"}
			case "changed overlap CWD":
				client.saved = []codex.ThreadMetadata{root}
				root.Cwd = "/workspace/work/another"
				client.metadata["root"] = root
			case "changed overlap source":
				client.saved = []codex.ThreadMetadata{root}
				root.Source = "exec"
				client.metadata["root"] = root
			case "changed member project":
				client.saved = []codex.ThreadMetadata{root, member}
				active["member"] = project
				otherProject := "other"
				member.ProjectID = &otherProject
				client.loadedIDs = append(client.loadedIDs, "member")
				client.metadata["member"] = member
			case "absent retained":
				client.loadedIDs = nil
			case "absent member":
				client.saved = []codex.ThreadMetadata{root}
				active["member"] = project
			}
			err := RequireExactActiveConversations(context.Background(), client, cwd, "root", active)
			wantSuccess := scenario == "loaded fresh root" || scenario == "matching overlap" || scenario == "other CWD"
			if (err == nil) != wantSuccess {
				t.Fatalf("proof=%v", err)
			}
			if len(client.options) == 0 {
				t.Fatal("retained discovery omitted the indexed active query")
			}
			for _, options := range client.options {
				if !options.UseStateDBOnly || options.Archived == nil || *options.Archived || options.Cwd != cwd || !reflect.DeepEqual(options.SourceKinds, ArchiveDiscoverySourceKinds()) {
					t.Fatalf("retained discovery changed its indexed active query: %#v", options)
				}
			}
			if client.deprecatedExcludeTurns {
				t.Fatal("loaded discovery supplied legacy excludeTurns instead of selected metadata-only defaults")
			}
		})
	}
}

func TestThreadlessObservationRechecksPublicOperationAfterDiscovery(t *testing.T) {
	client := &threadlessTestClient{}
	client.afterSaved = func() { client.binding = "new-directory-operation" }
	if err := RequireThreadlessConversations(context.Background(), client, "/workspace/work/example"); err == nil || len(client.lookups) != 2 {
		t.Fatalf("submission drift was accepted: %v, %#v", err, client.lookups)
	}
}
