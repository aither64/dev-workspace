// Package userstate owns the workspace namespaces in private user state.
package userstate

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// WorkspaceDirectory preserves the namespace used by existing package generations.
func WorkspaceDirectory(root, namespace, workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	id := filepath.Base(workspace) + "-" + hex.EncodeToString(digest[:8])
	return filepath.Join(root, namespace, id)
}
