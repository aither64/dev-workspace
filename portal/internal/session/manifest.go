package session

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	ManifestName    = "portal.yml"
	manifestMaxSize = 1024 * 1024
)

var slugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var gitHubPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var gitObjectPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var rfc3339Pattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`)
var legacyBooleanPattern = regexp.MustCompile(`(?i)^(?:yes|no|on|off)$`)
var lifecyclePattern = regexp.MustCompile(`\A---\r?\nlifecycle: (active|complete|abandoned)\r?\n---(?:\r?\n|\z)`)
var socketPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
var clientVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

type Codex struct {
	ThreadID      string `yaml:"thread_id,omitempty" json:"threadId,omitempty"`
	SocketPath    string `yaml:"socket_path,omitempty" json:"socketPath,omitempty"`
	ClientVersion string `yaml:"client_version,omitempty" json:"clientVersion,omitempty"`
}

type Creation struct {
	State           string `yaml:"state,omitempty" json:"state,omitempty"`
	InitialGoalSent bool   `yaml:"initial_goal_sent,omitempty" json:"initialGoalSent,omitempty"`
	GoalSHA256      string `yaml:"goal_sha256,omitempty" json:"goalSha256,omitempty"`
}

type Tmux struct {
	SocketPath         string `yaml:"socket_path,omitempty" json:"socketPath,omitempty"`
	CodexThreadID      string `yaml:"codex_thread_id,omitempty" json:"codexThreadId,omitempty"`
	CodexSocketPath    string `yaml:"codex_socket_path,omitempty" json:"codexSocketPath,omitempty"`
	CodexClientVersion string `yaml:"codex_client_version,omitempty" json:"codexClientVersion,omitempty"`
}

type Repository struct {
	Name           string `yaml:"name" json:"name"`
	Project        string `yaml:"project" json:"project"`
	GitHub         string `yaml:"github,omitempty" json:"github,omitempty"`
	Branch         string `yaml:"branch,omitempty" json:"branch,omitempty"`
	DefaultBranch  string `yaml:"default_branch,omitempty" json:"defaultBranch,omitempty"`
	InitialBaseSHA string `yaml:"initial_base_sha,omitempty" json:"initialBaseSha,omitempty"`
	FinalHeadSHA   string `yaml:"final_head_sha,omitempty" json:"finalHeadSha,omitempty"`
}

type Artifact struct {
	Label string `yaml:"label" json:"label"`
	Path  string `yaml:"path" json:"path"`
}

type Manifest struct {
	Schema       int          `yaml:"schema" json:"schema"`
	Slug         string       `yaml:"slug" json:"slug"`
	Codex        Codex        `yaml:"codex,omitempty" json:"codex"`
	Creation     Creation     `yaml:"creation,omitempty" json:"creation"`
	Repositories []Repository `yaml:"repositories,omitempty" json:"repositories"`
	Artifacts    []Artifact   `yaml:"artifacts,omitempty" json:"artifacts"`
	FinalizedAt  string       `yaml:"finalized_at,omitempty" json:"finalizedAt,omitempty"`
}

type Summary struct {
	Manifest
	Tmux              Tmux      `json:"tmux"`
	Archived          bool      `json:"archived"`
	Closed            bool      `json:"closed"`
	Interactive       bool      `json:"interactive"`
	PersistedThread   bool      `json:"-"`
	Lifecycle         string    `json:"lifecycle"`
	UpdatedAt         time.Time `json:"updatedAt"`
	ManifestUpdatedAt time.Time `json:"-"`
	Workspace         string    `json:"-"`
	Root              string    `json:"-"`
}

func ValidSlug(slug string) bool {
	return slugPattern.MatchString(slug)
}

func validSocketPath(path string) bool {
	return len(path) <= 4096 && socketPathPattern.MatchString(path) && filepath.Clean(path) == path
}

func (m *Manifest) Validate(expectedSlug string) error {
	if m.Schema != 1 {
		return fmt.Errorf("unsupported schema %d", m.Schema)
	}
	if !ValidSlug(m.Slug) {
		return errors.New("invalid slug")
	}
	if expectedSlug != "" && m.Slug != expectedSlug {
		return fmt.Errorf("manifest slug %q does not match directory %q", m.Slug, expectedSlug)
	}
	if m.Creation.State != "" && m.Creation.State != "creating" && m.Creation.State != "ready" {
		return fmt.Errorf("invalid creation state %q", m.Creation.State)
	}
	if m.Creation.GoalSHA256 != "" && !sha256Pattern.MatchString(m.Creation.GoalSHA256) {
		return errors.New("invalid creation goal digest")
	}
	if m.Codex.SocketPath != "" && !validSocketPath(m.Codex.SocketPath) {
		return errors.New("invalid Codex socket path")
	}
	if m.Codex.ClientVersion != "" && !clientVersionPattern.MatchString(m.Codex.ClientVersion) {
		return errors.New("invalid Codex client version")
	}
	if (m.Codex.SocketPath != "" || m.Codex.ClientVersion != "") &&
		(m.Codex.SocketPath == "" || m.Codex.ClientVersion == "" || m.Codex.ThreadID == "") {
		return errors.New("incomplete Codex provenance")
	}
	seenRepositories := make(map[string]struct{})
	for _, repository := range m.Repositories {
		if !ValidSlug(repository.Name) {
			return fmt.Errorf("invalid repository name %q", repository.Name)
		}
		if !ValidSlug(repository.Project) {
			return fmt.Errorf("invalid repository project %q", repository.Project)
		}
		if _, ok := seenRepositories[repository.Name]; ok {
			return fmt.Errorf("duplicate repository %q", repository.Name)
		}
		seenRepositories[repository.Name] = struct{}{}
		if repository.GitHub != "" && !gitHubPattern.MatchString(repository.GitHub) {
			return fmt.Errorf("invalid GitHub repository %q", repository.GitHub)
		}
		for name, value := range map[string]string{
			"initial_base_sha": repository.InitialBaseSHA,
			"final_head_sha":   repository.FinalHeadSHA,
		} {
			if value != "" && !gitObjectPattern.MatchString(value) {
				return fmt.Errorf("invalid repository %s", name)
			}
		}
	}
	seenArtifacts := make(map[string]struct{})
	for _, artifact := range m.Artifacts {
		if strings.TrimSpace(artifact.Label) == "" {
			return errors.New("artifact label is empty")
		}
		clean := filepath.Clean(artifact.Path)
		if artifact.Path == "" || filepath.IsAbs(artifact.Path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("artifact path escapes tracking directory: %q", artifact.Path)
		}
		if _, ok := seenArtifacts[clean]; ok {
			return fmt.Errorf("duplicate artifact path %q", clean)
		}
		seenArtifacts[clean] = struct{}{}
	}
	if m.FinalizedAt != "" {
		if !validRFC3339(m.FinalizedAt) {
			return errors.New("invalid finalized_at: expected an RFC3339 timestamp")
		}
		if _, err := time.Parse(time.RFC3339, m.FinalizedAt); err != nil {
			return fmt.Errorf("invalid finalized_at: %w", err)
		}
		for _, repository := range m.Repositories {
			if repository.InitialBaseSHA == "" || repository.FinalHeadSHA == "" {
				return fmt.Errorf("finalized repository %q has no immutable comparison", repository.Name)
			}
		}
	}
	return nil
}

func validRFC3339(value string) bool {
	if !rfc3339Pattern.MatchString(value) {
		return false
	}
	if strings.HasSuffix(value, "Z") {
		return true
	}
	offset := value[len(value)-6:]
	hour, hourErr := strconv.Atoi(offset[1:3])
	minute, minuteErr := strconv.Atoi(offset[4:6])
	return hourErr == nil && minuteErr == nil && hour < 24 && minute < 60
}

func List(workspace string) ([]Summary, error) {
	var summaries []Summary
	var problems []error
	seen := make(map[string]string)
	for _, root := range []struct {
		name     string
		archived bool
	}{{"work", false}, {"archive", true}} {
		base := filepath.Join(workspace, root.name)
		info, err := os.Lstat(base)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("inspect %s: %w", root.name, err))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			problems = append(problems, fmt.Errorf("%s root is not a real directory", root.name))
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", root.name, err))
			continue
		}
		for _, entry := range entries {
			if !ValidSlug(entry.Name()) {
				continue
			}
			entryInfo, err := entry.Info()
			if err != nil {
				problems = append(problems, fmt.Errorf("inspect %s/%s: %w", root.name, entry.Name(), err))
				continue
			}
			if entryInfo.Mode()&os.ModeSymlink != 0 {
				problems = append(problems, fmt.Errorf("%s/%s is a symlink", root.name, entry.Name()))
				continue
			}
			if !entryInfo.IsDir() {
				continue
			}
			summary, err := loadSummary(workspace, root.name, entry.Name(), root.archived)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if previous, ok := seen[summary.Slug]; ok {
				problems = append(problems, fmt.Errorf("duplicate session %q in %s and %s", summary.Slug, previous, root.name))
				continue
			}
			seen[summary.Slug] = root.name
			summaries = append(summaries, *summary)
		}
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Archived != summaries[j].Archived {
			return !summaries[i].Archived
		}
		return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
	})
	return summaries, errors.Join(problems...)
}

func Find(workspace, slug string) (*Summary, error) {
	if !ValidSlug(slug) {
		return nil, fs.ErrNotExist
	}
	var found *Summary
	for _, root := range []struct {
		name     string
		archived bool
	}{{"work", false}, {"archive", true}} {
		summary, err := loadSummary(workspace, root.name, slug, root.archived)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if found != nil {
			return nil, fmt.Errorf("duplicate session %q in work and archive", slug)
		}
		found = summary
	}
	if found == nil {
		return nil, fs.ErrNotExist
	}
	return found, nil
}

func loadSummary(workspace, root, slug string, archived bool) (*Summary, error) {
	relative := filepath.Join(root, slug, ManifestName)
	file, err := openConfined(workspace, relative, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > manifestMaxSize {
		return nil, fmt.Errorf("%s is not a regular manifest of at most 1 MiB", relative)
	}
	data, err := io.ReadAll(io.LimitReader(file, manifestMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > manifestMaxSize {
		return nil, fmt.Errorf("%s exceeds 1 MiB", relative)
	}
	var manifest Manifest
	if err := decodeManifest(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", relative, err)
	}
	if err := manifest.Validate(slug); err != nil {
		return nil, fmt.Errorf("validate %s: %w", relative, err)
	}
	lifecycle, stateUpdatedAt, err := loadLifecycle(workspace, root, slug)
	if err != nil {
		return nil, fmt.Errorf("validate %s/%s/state.md: %w", root, slug, err)
	}
	planUpdatedAt, err := trackingModTime(workspace, root, slug, "plan.md")
	if err != nil {
		return nil, fmt.Errorf("validate %s/%s/plan.md: %w", root, slug, err)
	}
	terminal := lifecycle == "complete" || lifecycle == "abandoned"
	if archived && (!terminal || manifest.FinalizedAt == "") {
		return nil, fmt.Errorf("validate %s: archived session lacks terminal lifecycle or finalization metadata", relative)
	}
	if !archived && lifecycle == "active" && manifest.FinalizedAt != "" {
		return nil, fmt.Errorf("validate %s: active session contains finalization metadata", relative)
	}
	closed := terminal
	updatedAt := info.ModTime()
	if stateUpdatedAt.After(updatedAt) {
		updatedAt = stateUpdatedAt
	}
	if planUpdatedAt.After(updatedAt) {
		updatedAt = planUpdatedAt
	}
	if manifest.FinalizedAt != "" {
		updatedAt, _ = time.Parse(time.RFC3339, manifest.FinalizedAt)
	}
	return &Summary{
		Manifest: manifest, Archived: archived, Closed: closed, Interactive: false,
		PersistedThread: manifest.Codex.ThreadID != "",
		Lifecycle:       lifecycle, UpdatedAt: updatedAt, ManifestUpdatedAt: info.ModTime(),
		Workspace: workspace, Root: root,
	}, nil
}

func loadLifecycle(workspace, root, slug string) (string, time.Time, error) {
	relative := filepath.Join(root, slug, "state.md")
	file, err := openConfined(workspace, relative, unix.O_RDONLY, 0)
	if err != nil {
		return "", time.Time{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > manifestMaxSize {
		return "", time.Time{}, errors.New("state file is not a regular file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, manifestMaxSize+1))
	if err != nil {
		return "", time.Time{}, err
	}
	if len(data) > manifestMaxSize {
		return "", time.Time{}, errors.New("state file exceeds 1 MiB")
	}
	if !utf8.Valid(data) {
		return "", time.Time{}, errors.New("state file is not valid UTF-8")
	}
	match := lifecyclePattern.FindSubmatch(data)
	if match == nil {
		return "", time.Time{}, errors.New("state file has no anchored lifecycle front matter")
	}
	return string(match[1]), info.ModTime(), nil
}

func trackingModTime(workspace, root, slug, name string) (time.Time, error) {
	file, err := openConfined(workspace, filepath.Join(root, slug, name), unix.O_RDONLY, 0)
	if err != nil {
		return time.Time{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > manifestMaxSize {
		return time.Time{}, fmt.Errorf("%s is not a regular file of at most 1 MiB", name)
	}
	return info.ModTime(), nil
}

func decodeManifest(data []byte, manifest *Manifest) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("manifest must contain one mapping")
	}
	if err := validateManifestNode(document.Content[0]); err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(manifest); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest contains multiple documents")
		}
		return err
	}
	return nil
}

func validateManifestNode(root *yaml.Node) error {
	if err := rejectAliasesAndDuplicateKeys(root); err != nil {
		return err
	}
	fields, err := strictMapping(root, []string{
		"schema", "slug", "codex", "creation", "repositories", "artifacts", "finalized_at",
	}, []string{"schema", "slug"})
	if err != nil {
		return err
	}
	if err := scalarTag(fields["schema"], "!!int", "schema"); err != nil {
		return err
	}
	if err := scalarTag(fields["slug"], "!!str", "slug"); err != nil {
		return err
	}
	if node := fields["codex"]; node != nil {
		items, err := strictMapping(node, []string{"thread_id", "socket_path", "client_version"}, nil)
		if err != nil {
			return fmt.Errorf("codex: %w", err)
		}
		if err := optionalString(items, "thread_id"); err != nil {
			return fmt.Errorf("codex: %w", err)
		}
		for _, key := range []string{"socket_path", "client_version"} {
			if err := optionalString(items, key); err != nil {
				return fmt.Errorf("codex: %w", err)
			}
			if value := items[key]; value != nil && value.Value == "" {
				return fmt.Errorf("codex: %s must not be empty", key)
			}
		}
	}
	if node := fields["creation"]; node != nil {
		items, err := strictMapping(node, []string{"state", "initial_goal_sent", "goal_sha256"}, nil)
		if err != nil {
			return fmt.Errorf("creation: %w", err)
		}
		if err := optionalString(items, "state"); err != nil {
			return fmt.Errorf("creation: %w", err)
		}
		if value := items["state"]; value != nil && value.Value == "" {
			return errors.New("creation: state must not be empty")
		}
		if value := items["initial_goal_sent"]; value != nil {
			if err := scalarTag(value, "!!bool", "initial_goal_sent"); err != nil {
				return fmt.Errorf("creation: %w", err)
			}
		}
		if err := optionalString(items, "goal_sha256"); err != nil {
			return fmt.Errorf("creation: %w", err)
		}
		if value := items["goal_sha256"]; value != nil && value.Value == "" {
			return errors.New("creation: goal_sha256 must not be empty")
		}
	}
	if node := fields["repositories"]; node != nil {
		if node.Kind != yaml.SequenceNode {
			return errors.New("repositories must be a sequence")
		}
		for index, repository := range node.Content {
			items, err := strictMapping(repository, []string{
				"name", "project", "github", "branch", "default_branch", "initial_base_sha", "final_head_sha",
			}, []string{"name", "project"})
			if err != nil {
				return fmt.Errorf("repository %d: %w", index, err)
			}
			for _, key := range []string{"name", "project", "github", "branch", "default_branch", "initial_base_sha", "final_head_sha"} {
				if err := optionalString(items, key); err != nil {
					return fmt.Errorf("repository %d: %w", index, err)
				}
			}
			for _, key := range []string{"github", "initial_base_sha", "final_head_sha"} {
				if value := items[key]; value != nil && value.Value == "" {
					return fmt.Errorf("repository %d: %s must not be empty", index, key)
				}
			}
		}
	}
	if node := fields["artifacts"]; node != nil {
		if node.Kind != yaml.SequenceNode {
			return errors.New("artifacts must be a sequence")
		}
		for index, artifact := range node.Content {
			items, err := strictMapping(artifact, []string{"label", "path"}, []string{"label", "path"})
			if err != nil {
				return fmt.Errorf("artifact %d: %w", index, err)
			}
			for _, key := range []string{"label", "path"} {
				if err := optionalString(items, key); err != nil {
					return fmt.Errorf("artifact %d: %w", index, err)
				}
			}
		}
	}
	if err := optionalString(fields, "finalized_at"); err != nil {
		return err
	}
	if value := fields["finalized_at"]; value != nil && value.Value == "" {
		return errors.New("finalized_at must not be empty")
	}
	return nil
}

func rejectAliasesAndDuplicateKeys(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return errors.New("YAML aliases are not supported")
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return errors.New("explicit YAML tags are not supported")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("mapping keys must be strings")
			}
			if _, ok := seen[key.Value]; ok {
				return fmt.Errorf("duplicate mapping key %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := rejectAliasesAndDuplicateKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func strictMapping(node *yaml.Node, allowed, required []string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("value must be a mapping")
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	result := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index].Value
		if _, ok := allowedSet[key]; !ok {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		result[key] = node.Content[index+1]
	}
	for _, key := range required {
		if result[key] == nil {
			return nil, fmt.Errorf("missing field %q", key)
		}
	}
	return result, nil
}

func optionalString(fields map[string]*yaml.Node, key string) error {
	if node := fields[key]; node != nil {
		return scalarTag(node, "!!str", key)
	}
	return nil
}

func scalarTag(node *yaml.Node, tag, name string) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != tag {
		return fmt.Errorf("%s must be a %s scalar", name, strings.TrimPrefix(tag, "!!"))
	}
	if tag == "!!str" && node.Style == 0 && legacyBooleanPattern.MatchString(node.Value) {
		return fmt.Errorf("%s must quote YAML 1.1 boolean-like strings", name)
	}
	return nil
}

func OpenArtifact(summary *Summary, artifactPath string, maxSize int64) (*os.File, os.FileInfo, error) {
	allowed := artifactPath == "plan.md" || artifactPath == "state.md"
	if !allowed {
		for _, artifact := range summary.Artifacts {
			if filepath.Clean(artifact.Path) == filepath.Clean(artifactPath) {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		return nil, nil, fs.ErrPermission
	}
	clean := filepath.Clean(artifactPath)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, nil, fs.ErrPermission
	}
	relative := filepath.Join(summary.Root, summary.Slug, clean)
	file, err := openConfined(summary.Workspace, relative, unix.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, fs.ErrPermission
	}
	if info.Size() > maxSize {
		file.Close()
		return nil, nil, fmt.Errorf("artifact exceeds %d bytes", maxSize)
	}
	return file, info, nil
}

func openConfined(workspace, relative string, flags int, mode uint32) (*os.File, error) {
	clean := filepath.Clean(relative)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fs.ErrPermission
	}
	root, err := unix.Open(workspace, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(root)
	fd, err := unix.Openat2(root, clean, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Mode:    uint64(mode),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), clean), nil
}
