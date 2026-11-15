package piruntime

import (
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const installedModelsPath = "share/dev-workspace/pi-models.json"
const installedModelsLimit = 64 << 10

// InstalledModelConfiguration is optional immutable package input, not member
// settings or credential configuration. Production supplies the admitted app root.
type InstalledModelConfiguration struct {
	Path    string
	BaseURL string
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
				BaseURL string `json:"baseUrl"`
				API     string `json:"api"`
			} `json:"openai"`
		} `json:"providers"`
	}
	if err != nil || strictDecode(data, &config, installedModelsLimit) != nil || config.Providers.OpenAI.API != "openai-responses" {
		return InstalledModelConfiguration{}, ErrScope
	}
	endpoint := config.Providers.OpenAI.BaseURL
	u, err := url.Parse(endpoint)
	if err != nil || !validString(endpoint) || !validString(u.Path) ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		strings.ContainsAny(endpoint, "?#$`{}\\ \t\r\n") {
		return InstalledModelConfiguration{}, ErrScope
	}
	return InstalledModelConfiguration{Path: path, BaseURL: endpoint}, nil
}
