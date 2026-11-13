package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// UtilityRetention is an observational compatibility obligation. Receipt input
// remains typed Go data for the owning preflight join, never CLI presentation.
type UtilityRetention struct {
	Utility                   UtilityRef
	CreationID, Generation    string
	Settings, InitialSettings EffectiveSettings
	Receipts                  []UtilitySubmissionStatus
}

// InspectRetainedUtilities does not construct a Manager or touch native state.
// Missing namespaces are absence; contradictory or orphan records refuse with
// no partial inventory and no repair, lock, directory or file creation.
func InspectRetainedUtilities(ctx context.Context, userStateRoot string, workspaces []string) ([]UtilityRetention, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := ownedDirectory(userStateRoot)
	if err != nil || root != userStateRoot {
		return nil, ErrScope
	}
	if len(workspaces) == 0 || len(workspaces) > 1024 {
		return nil, ErrScope
	}
	namespaces := map[string]string{}
	for _, workspace := range workspaces {
		canonical, err := filepath.EvalSymlinks(workspace)
		if err != nil || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || canonical != workspace {
			return nil, ErrScope
		}
		info, err := os.Stat(workspace)
		if err != nil || !info.IsDir() {
			return nil, ErrScope
		}
		sum := sha256.Sum256([]byte(workspace))
		key := hex.EncodeToString(sum[:])
		if namespaces[key] != "" {
			return nil, ErrScope
		}
		namespaces[key] = workspace
	}
	readDir := func(path string) ([]os.DirEntry, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, ErrConflict
		}
		return os.ReadDir(path)
	}
	result := []UtilityRetention{}
	count := 0
	runtimeRoot := filepath.Join(root, "pi-runtime")
	namespaceEntries, err := readDir(runtimeRoot)
	if err != nil {
		return nil, err
	}
	for _, namespace := range namespaceEntries {
		if !namespace.IsDir() || namespace.Type()&os.ModeSymlink != 0 || !hexPattern.MatchString(namespace.Name()) {
			return nil, ErrConflict
		}
		slugs, err := readDir(filepath.Join(runtimeRoot, namespace.Name()))
		if err != nil {
			return nil, err
		}
		for _, slug := range slugs {
			if !slug.IsDir() || slug.Type()&os.ModeSymlink != 0 || !slugPattern.MatchString(slug.Name()) {
				return nil, ErrConflict
			}
			incarnations, err := readDir(filepath.Join(runtimeRoot, namespace.Name(), slug.Name()))
			if err != nil {
				return nil, err
			}
			for _, incarnation := range incarnations {
				if !incarnation.IsDir() || incarnation.Type()&os.ModeSymlink != 0 || !hexPattern.MatchString(incarnation.Name()) {
					return nil, ErrConflict
				}
				session := agentbackend.SessionRef{Workspace: namespaces[namespace.Name()], Slug: slug.Name(), Incarnation: incarnation.Name()}
				directory := filepath.Join(runtimeRoot, namespace.Name(), slug.Name(), incarnation.Name())
				operations, err := readDir(filepath.Join(directory, "utilities"))
				if err != nil {
					return nil, err
				}
				if len(operations) == 0 {
					continue
				}
				if session.Workspace == "" {
					return nil, ErrScope
				}
				expected, err := stateDirectory(root, session)
				if err != nil || expected != directory {
					return nil, ErrScope
				}
				for _, operation := range operations {
					if !operation.IsDir() || operation.Type()&os.ModeSymlink != 0 || !operationPattern.MatchString(operation.Name()) {
						return nil, ErrConflict
					}
					utilities, err := readDir(filepath.Join(directory, "utilities", operation.Name()))
					if err != nil {
						return nil, err
					}
					if len(utilities) == 0 {
						return nil, ErrConflict
					}
					for _, incarnation := range utilities {
						if !incarnation.IsDir() || incarnation.Type()&os.ModeSymlink != 0 || !hexPattern.MatchString(incarnation.Name()) {
							return nil, ErrConflict
						}
						utilityRoot := filepath.Join(directory, "utilities", operation.Name(), incarnation.Name())
						count++
						if count > 65536 {
							return nil, ErrScope
						}
						var mapping conversationRecord
						if err := readPrivate(filepath.Join(utilityRoot, "conversation.json"), &mapping); err != nil {
							return nil, err
						}
						if mapping.Utility == nil || mapping.Utility.Session != session || mapping.Utility.OperationID != operation.Name() || mapping.Utility.Incarnation != incarnation.Name() {
							return nil, ErrConflict
						}
						if err := validateUtilityRecord(mapping, *mapping.Utility, utilityRoot); err != nil {
							return nil, err
						}
						artifacts, err := readDir(utilityRoot)
						if err != nil {
							return nil, err
						}
						for _, artifact := range artifacts {
							if artifact.Type()&os.ModeSymlink != 0 {
								return nil, ErrConflict
							}
							if artifact.Name() == "conversation.json" {
								if artifact.IsDir() {
									return nil, ErrConflict
								}
								continue
							}
							if !artifact.IsDir() || (artifact.Name() != "agent" && artifact.Name() != "history" && artifact.Name() != "submissions") {
								return nil, ErrConflict
							}
						}
						retained := UtilityRetention{Utility: *mapping.Utility, CreationID: mapping.CreationID, Generation: mapping.Generation, Settings: mapping.Settings, InitialSettings: mapping.InitialSettings, Receipts: []UtilitySubmissionStatus{}}
						receipts, err := readDir(filepath.Join(utilityRoot, "submissions"))
						if err != nil {
							return nil, err
						}
						engine := receiptEngine{root: filepath.Join(utilityRoot, "submissions"), ownerKind: "utility"}
						for _, receipt := range receipts {
							count++
							if count > 65536 {
								return nil, ErrScope
							}
							if receipt.IsDir() || receipt.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(receipt.Name(), ".json") {
								return nil, ErrConflict
							}
							id := strings.TrimSuffix(receipt.Name(), ".json")
							status, err := engine.loadOwned(id)
							if err != nil {
								return nil, err
							}
							if status.ref.utility == nil || status.ref.utility.Utility != retained.Utility || !hexPattern.MatchString(status.Generation) || validateSettings(status.Settings) != nil {
								return nil, ErrConflict
							}
							retained.Receipts = append(retained.Receipts, status.utility())
						}
						result = append(result, retained)
					}
				}
			}
		}
	}
	return result, nil
}
