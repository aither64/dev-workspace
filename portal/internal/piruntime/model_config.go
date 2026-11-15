package piruntime

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const installedModelsPath = "share/dev-workspace/pi-models.json"
const installedModelsLimit = 64 << 10

var installedModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// InstalledModelConfiguration is optional immutable package input, not member
// settings or credential configuration. Production supplies the admitted app root.
type InstalledModelConfiguration struct {
	Path    string
	BaseURL string
	Model   string
}

// LoadInstalledModelConfiguration never writes or consults a moving profile.
// Absence preserves the legacy private models file; every other error refuses.
func LoadInstalledModelConfiguration(app string) (InstalledModelConfiguration, error) {
	if !validString(app) || !filepath.IsAbs(app) || filepath.Clean(app) != app {
		return InstalledModelConfiguration{}, ErrScope
	}
	path := filepath.Join(app, installedModelsPath)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return InstalledModelConfiguration{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > installedModelsLimit {
		return InstalledModelConfiguration{}, ErrScope
	}
	f, err := os.Open(path)
	if err != nil {
		return InstalledModelConfiguration{}, ErrScope
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, installedModelsLimit+1))
	var config struct {
		Providers struct {
			OpenAI struct {
				BaseURL        string          `json:"baseUrl"`
				API            string          `json:"api"`
				ModelOverrides json.RawMessage `json:"modelOverrides,omitempty"`
			} `json:"openai"`
		} `json:"providers"`
	}
	if err != nil || strictDecode(data, &config, installedModelsLimit) != nil || config.Providers.OpenAI.API != "openai-responses" {
		return InstalledModelConfiguration{}, ErrScope
	}
	model := ""
	if raw := config.Providers.OpenAI.ModelOverrides; len(raw) != 0 {
		var overrides map[string]struct {
			SamplingParams struct {
				ParallelToolCalls *bool `json:"parallel_tool_calls"`
			} `json:"samplingParams"`
		}
		if strictDecode(raw, &overrides, installedModelsLimit) != nil || len(overrides) != 1 {
			return InstalledModelConfiguration{}, ErrScope
		}
		for name, override := range overrides {
			if !installedModelID.MatchString(name) || override.SamplingParams.ParallelToolCalls == nil {
				return InstalledModelConfiguration{}, ErrScope
			}
			model = name
		}
	}
	endpoint := config.Providers.OpenAI.BaseURL
	u, err := url.Parse(endpoint)
	if err != nil || !validString(endpoint) || !validString(u.Path) ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		strings.ContainsAny(endpoint, "?#$`{}\\ \t\r\n") {
		return InstalledModelConfiguration{}, ErrScope
	}
	return InstalledModelConfiguration{Path: path, BaseURL: endpoint, Model: model}, nil
}
