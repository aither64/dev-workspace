package session

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SensitiveInstructions refreshes with each technical session binding, including
// resumed and forked conversations. Delegated work must carry this policy too.
func SensitiveInstructions(workspace, slug string) string {
	for _, root := range []string{"work", "archive"} {
		data, err := os.ReadFile(filepath.Join(workspace, root, slug, ManifestName))
		if err != nil {
			continue
		}
		var marker struct {
			Sensitive bool `yaml:"sensitive"`
		}
		if yaml.Unmarshal(data, &marker) == nil && marker.Sensitive {
			return fmt.Sprintf("This is a sensitive session. Keep its plans, state, notes, review evidence and handoffs locally under %s. Never stage or commit these records, copy them into tracked notes or other sessions, or force-add ignored records. Maintain substantive local tracking without the ordinary initial/checkpoint/archive tracking commits. Project code retains its normal Git workflow. Pass this sensitivity instruction to any delegated agent.\n\n", filepath.Join(workspace, "work", slug))
		}
	}
	return ""
}
