package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

// InspectRetainedPiLifecycles reads only the existing lifecycle journals and
// recovery copies. A full SessionRef keeps a removed incarnation distinct from
// a later explicit creation with the same slug.
func InspectRetainedPiLifecycles(ctx context.Context, root, workspace string) ([]PiLifecycleCapture, error) {
	result := []PiLifecycleCapture{}
	visited := 0
	read := func(path, slug string, piSchema int, terminal bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 65536 {
			return errors.New("Pi lifecycle inventory exceeds limit")
		}
		data, err := readPrivateLifecycleRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := DecodePiCreationJSON(data, &fields, maxLifecycleJournalBytes); err != nil {
			return err
		}
		if _, pi := fields["pi"]; !pi {
			return nil
		}
		var identity struct {
			Schema                                     int
			Slug, Workspace, Phase, State, OperationID string
		}
		if json.Unmarshal(data, &identity) != nil {
			return errors.New("invalid Pi lifecycle record")
		}
		// operation_id is deliberately decoded with its actual journal spelling.
		var operation string
		_ = json.Unmarshal(fields["operation_id"], &operation)
		if identity.Schema != piSchema || identity.Workspace != workspace || identity.Slug != slug || !validLifecycleJournalID(operation) {
			return errors.New("Pi lifecycle recovery scope changed")
		}
		p, err := DecodePiLifecycleCapture(fields["pi"], workspace, slug)
		if err != nil {
			return err
		}
		if terminal && (p.Terminal == nil || (piSchema == 5 && identity.Phase != "archived") || (piSchema == 2 && (identity.Phase != "removed" || identity.State != "removed"))) {
			return errors.New("Pi lifecycle recovery is not terminal")
		}
		result = append(result, p)
		return nil
	}
	locks := filepath.Join(workspace, "worktrees", ".locks")
	entries, err := os.ReadDir(locks)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		for _, spec := range []struct {
			suffix string
			schema int
		}{{".archive.json", 5}, {".removal.json", 2}, {".revive.json", 6}} {
			if strings.HasSuffix(entry.Name(), spec.suffix) {
				slug := strings.TrimSuffix(entry.Name(), spec.suffix)
				if !ValidSlug(slug) {
					return nil, errors.New("invalid Pi lifecycle slug")
				}
				if err := read(filepath.Join(locks, entry.Name()), slug, spec.schema, false); err != nil {
					return nil, err
				}
			}
		}
	}
	archive := userstate.WorkspaceDirectory(root, "archived-worktrees", workspace)
	slugs, err := os.ReadDir(archive)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, slug := range slugs {
		if !ValidSlug(slug.Name()) {
			return nil, errors.New("invalid Pi archive scope")
		}
		operations, err := os.ReadDir(filepath.Join(archive, slug.Name()))
		if err != nil {
			return nil, err
		}
		for _, op := range operations {
			if !validLifecycleJournalID(op.Name()) {
				return nil, errors.New("invalid Pi archive operation")
			}
			if err := read(filepath.Join(archive, slug.Name(), op.Name(), "lifecycle.json"), slug.Name(), 5, true); err != nil {
				return nil, err
			}
		}
	}
	removed := userstate.WorkspaceDirectory(root, "removed", workspace)
	removals, err := os.ReadDir(removed)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range removals {
		path := filepath.Join(removed, entry.Name(), "recovery.json")
		data, err := readPrivateLifecycleRecord(path)
		if err != nil {
			return nil, err
		}
		var identity struct{ Slug string }
		if json.Unmarshal(data, &identity) != nil || !ValidSlug(identity.Slug) {
			return nil, errors.New("invalid removal scope")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return nil, errors.New("invalid removal record")
		}
		if _, pi := fields["pi"]; !pi {
			continue
		}
		var phase string
		_ = json.Unmarshal(fields["phase"], &phase)
		if err := read(path, identity.Slug, 2, phase == "removed"); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func readPrivateLifecycleRecord(path string) ([]byte, error) {
	// Reuse the existing bounded owner/mode/no-follow backend record reader.
	return readBackendRecord(filepath.Dir(path), filepath.Base(path))
}
