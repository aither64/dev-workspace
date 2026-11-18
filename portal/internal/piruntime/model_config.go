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
	Models  []InstalledPhysicalModel
}

type InstalledPhysicalModel struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	ContextWindow  int      `json:"contextWindow"`
	Reasoning      bool     `json:"reasoning"`
	Input          []string `json:"input"`
	SamplingParams struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
	} `json:"samplingParams"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap"`
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
				BaseURL        string                   `json:"baseUrl"`
				API            string                   `json:"api"`
				ModelOverrides json.RawMessage          `json:"modelOverrides,omitempty"`
				Models         []InstalledPhysicalModel `json:"models,omitempty"`
			} `json:"openai"`
		} `json:"providers"`
	}
	if err != nil || strictDecode(data, &config, installedModelsLimit) != nil || config.Providers.OpenAI.API != "openai-responses" {
		return InstalledModelConfiguration{}, ErrScope
	}
	model := ""
	seen := map[string]bool{}
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
			seen[name] = true
		}
	}
	if config.Providers.OpenAI.Models != nil {
		if len(config.Providers.OpenAI.Models) == 0 || len(config.Providers.OpenAI.Models) > 16 {
			return InstalledModelConfiguration{}, ErrScope
		}
		for _, physical := range config.Providers.OpenAI.Models {
			if !installedModelID.MatchString(physical.ID) || seen[physical.ID] || !validString(physical.Name) ||
				physical.ContextWindow <= 0 || physical.ContextWindow > 2000000 || !physical.Reasoning ||
				len(physical.Input) != 2 || physical.Input[0] != "text" || physical.Input[1] != "image" ||
				physical.SamplingParams.ParallelToolCalls == nil || *physical.SamplingParams.ParallelToolCalls ||
				len(physical.ThinkingLevelMap) != 7 {
				return InstalledModelConfiguration{}, ErrScope
			}
			for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
				value := physical.ThinkingLevelMap[level]
				if value == nil || *value != level {
					return InstalledModelConfiguration{}, ErrScope
				}
			}
			for _, level := range []string{"off", "minimal"} {
				value, exists := physical.ThinkingLevelMap[level]
				if !exists || value != nil {
					return InstalledModelConfiguration{}, ErrScope
				}
			}
			seen[physical.ID] = true
		}
	}
	endpoint := config.Providers.OpenAI.BaseURL
	u, err := url.Parse(endpoint)
	if err != nil || !validString(endpoint) || !validString(u.Path) ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		strings.ContainsAny(endpoint, "?#$`{}\\ \t\r\n") {
		return InstalledModelConfiguration{}, ErrScope
	}
	return InstalledModelConfiguration{Path: path, BaseURL: endpoint, Model: model, Models: config.Providers.OpenAI.Models}, nil
}
