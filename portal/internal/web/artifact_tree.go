package web

import (
	"github.com/aither64/dev-workspace/portal/internal/session"
	"strings"
)

type artifactNode struct {
	Name      string
	Directory string
	Artifact  *session.Artifact
	Children  []*artifactNode
}

// The manifest reader owns normalization/deduplication. Keep its order and labels
// while grouping files by their relative directory for native disclosures.
func artifactTree(artifacts []session.Artifact) []*artifactNode {
	root := &artifactNode{}
	directories := map[string]*artifactNode{"": root}
	for _, item := range artifacts {
		parent, directory := root, ""
		parts := strings.Split(item.Path, "/")
		for _, part := range parts[:len(parts)-1] {
			if directory != "" {
				directory += "/"
			}
			directory += part
			child := directories[directory]
			if child == nil {
				child = &artifactNode{Name: part, Directory: directory}
				directories[directory] = child
				parent.Children = append(parent.Children, child)
			}
			parent = child
		}
		artifact := item
		parent.Children = append(parent.Children, &artifactNode{Name: parts[len(parts)-1], Artifact: &artifact})
	}
	return root.Children
}
