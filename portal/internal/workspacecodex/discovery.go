package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"unicode"
	"unicode/utf8"

	"github.com/aither64/codex-web/codex"
)

type ArchiveDiscoveryClient interface {
	ListThreads(context.Context, codex.ThreadListOptions) ([]codex.ThreadMetadata, *string, error)
	LoadedThreadIDs(context.Context) ([]string, error)
	ReadThreadMetadata(context.Context, string, bool) (codex.ThreadMetadata, error)
}

type ThreadlessObservationClient interface {
	ArchiveDiscoveryClient
	ThreadOperationAttempt(string) (string, error)
}

// RequireThreadlessConversations proves only the public directory operation
// binding and complete native discovery. It never enumerates a private ledger
// or adopts an identity returned by either owner.
func RequireThreadlessConversations(ctx context.Context, client ThreadlessObservationClient, cwd string) error {
	checkOperation := func() error {
		thread, err := client.ThreadOperationAttempt(cwd)
		if err != nil {
			return observationFailure("submission_unverified", "Directory submission proof cannot be verified.")
		}
		if thread != "" {
			return observationFailure("submission_residue", "A directory operation binding contradicts threadless tracking.")
		}
		return nil
	}
	if err := checkOperation(); err != nil {
		return err
	}
	if err := RequireNoConversations(ctx, client, cwd); err != nil {
		return err
	}
	return checkOperation()
}

// RequireExactActiveConversations owns the complete selected-source discovery
// rule shared by retirement and semantic observation. It confirms a retained
// set; it never adopts a discovered identity.
func RequireExactActiveConversations(ctx context.Context, client ArchiveDiscoveryClient, cwd, rootThreadID string, active map[string]string) error {
	archived := false
	seen, cursors := map[string]codex.ThreadMetadata{}, map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > len(active) {
			return errors.New("active archive discovery exceeded the retained set")
		}
		threads, next, err := client.ListThreads(ctx, codex.ThreadListOptions{
			Cwd: cwd, Archived: &archived, Limit: len(active) + 1, SortDirection: "asc", Cursor: cursor,
			SourceKinds: ArchiveDiscoverySourceKinds(), UseStateDBOnly: true,
		})
		if err != nil {
			return fmt.Errorf("discover active archive conversations: %w", err)
		}
		for _, thread := range threads {
			project, retained := active[thread.ID]
			_, duplicate := seen[thread.ID]
			if !retained || duplicate || thread.Cwd != cwd ||
				(thread.ID == rootThreadID && thread.Source != "vscode") ||
				(project != "" && (thread.ProjectID == nil || *thread.ProjectID != project)) {
				return errors.New("unknown or changed Codex thread uses the session directory; refusing archive")
			}
			seen[thread.ID] = thread
		}
		if next == nil {
			break
		}
		if *next == "" || cursors[*next] {
			return errors.New("active archive discovery returned an invalid cursor")
		}
		cursors[*next], cursor = true, *next
	}
	if err := requireLoadedConversationScope(ctx, client, cwd, rootThreadID, active, seen); err != nil {
		return err
	}
	if len(seen) != len(active) {
		return errors.New("a retained active conversation is absent from archive discovery")
	}
	return nil
}

// Saved-list enrichment does not add a loaded-only thread to that fixed slice.
// Read exact metadata for every public manager ID to establish its CWD; failed
// reads are unknown, and an unrelated positively identified CWD is harmless.
func requireLoadedConversationScope(ctx context.Context, client ArchiveDiscoveryClient, cwd, rootID string, retained map[string]string, seen map[string]codex.ThreadMetadata) error {
	ids, err := client.LoadedThreadIDs(ctx)
	if err != nil {
		return observationFailure("loaded_discovery_unverified", "Loaded conversation discovery cannot be verified.")
	}
	loaded := map[string]bool{}
	for _, id := range ids {
		if !observationID(id, true) || loaded[id] {
			return observationFailure("loaded_identity_unverified", "Loaded conversation identity is invalid or repeated.")
		}
		loaded[id] = true
		metadata, err := client.ReadThreadMetadata(ctx, id, false)
		if err != nil || metadata.ID != id || !canonicalObservationCWD(metadata.Cwd) {
			return observationFailure("loaded_identity_unverified", "Loaded conversation directory cannot be verified.")
		}
		project, known := retained[id]
		if metadata.Cwd != cwd && !known {
			continue
		}
		if !known || metadata.Cwd != cwd || id == rootID && metadata.Source != "vscode" || project != "" && (metadata.ProjectID == nil || *metadata.ProjectID != project) {
			return observationFailure("loaded_conversation_residue", "An unknown or changed loaded conversation uses the session directory.")
		}
		if saved, overlap := seen[id]; overlap && (saved.Cwd != metadata.Cwd || saved.Source != metadata.Source || !reflect.DeepEqual(saved.ProjectID, metadata.ProjectID)) {
			return observationFailure("loaded_identity_unverified", "Saved and loaded conversation identities disagree.")
		}
		seen[id] = metadata
	}
	return nil
}

func canonicalObservationCWD(cwd string) bool {
	if len(cwd) > 4096 || !utf8.ValidString(cwd) || !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return false
	}
	for _, r := range cwd {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Threadless permission is a fresh negative proof, including archived and
// noninteractive sources. Any page/cursor means absence has not been proved.
func RequireNoConversations(ctx context.Context, client ArchiveDiscoveryClient, cwd string) error {
	for _, archived := range []bool{false, true} {
		threads, next, err := client.ListThreads(ctx, codex.ThreadListOptions{
			Cwd: cwd, Archived: &archived, SourceKinds: ArchiveDiscoverySourceKinds(), Limit: 1, SortDirection: "asc",
		})
		if err != nil {
			return observationFailure("absence_unavailable", "Conversation absence cannot be verified.")
		}
		if len(threads) != 0 || next != nil {
			return observationFailure("conversation_residue", "A conversation or incomplete discovery contradicts threadless tracking.")
		}
	}
	return requireLoadedConversationScope(ctx, client, cwd, "", map[string]string{}, map[string]codex.ThreadMetadata{})
}
