package agentteams

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// VerificationWatcherInstructions reads only our generated native adapter
// format. Its values are JSON strings; accepting arbitrary TOML is unnecessary.
func VerificationWatcherInstructions(packageRoot string, catalog Catalog) (string, error) {
	if err := catalog.Validate(); err != nil {
		return "", err
	}
	if len(catalog.NativeAgentConfigs.Utilities) != 1 {
		return "", errors.New("verification utility is not unique")
	}
	variant := catalog.NativeAgentConfigs.Utilities[0]
	data, err := readPackageFile(packageRoot, variant.Path, 64*1024)
	if err != nil {
		return "", err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok {
			return "", errors.New("invalid generated utility configuration")
		}
		switch key {
		case "name", "description", "developer_instructions", "sandbox_mode", "approval_policy":
		default:
			return "", errors.New("unexpected generated utility field")
		}
		if _, found := fields[key]; found {
			return "", errors.New("duplicate generated utility field")
		}
		var value string
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &value); err != nil {
			return "", errors.New("utility configuration value is not a generated JSON string")
		}
		fields[key] = value
	}
	instructions := fields["developer_instructions"]
	if fields["name"] != variant.Name || instructions == "" ||
		fmt.Sprintf("%x", sha256.Sum256([]byte(instructions))) != variant.Identity.BehaviorDigest {
		return "", errors.New("utility configuration does not match its catalog identity")
	}
	if catalog.FullExecutionAccess() && (fields["sandbox_mode"] != "danger-full-access" || fields["approval_policy"] != "never") {
		return "", errors.New("utility configuration execution policy differs")
	}
	return instructions, nil
}
