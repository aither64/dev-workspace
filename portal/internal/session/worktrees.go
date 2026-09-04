package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ActiveRepositories returns the manifest repositories plus worktrees that Git
// currently registers below this initiative's worktree directory. Repository
// identity and remote metadata are read only from the workspace checkout or a
// canonical bare repository, never from the writable worktree itself.
func ActiveRepositories(workspace, slug string, registered []Repository) ([]Repository, error) {
	if !ValidSlug(slug) {
		return nil, errors.New("invalid session slug")
	}

	discovered, err := DiscoverActiveRepositories(workspace)
	result, mergeErr := MergeActiveRepositories(registered, discovered[slug])
	return result, errors.Join(err, mergeErr)
}

// MergeActiveRepositories adds verified live worktrees to a manifest without
// allowing them to silently replace conflicting registered metadata.
func MergeActiveRepositories(registered, discovered []Repository) ([]Repository, error) {
	result := append([]Repository(nil), registered...)
	byName := make(map[string]Repository, len(result))
	for _, repository := range result {
		byName[repository.Name] = repository
	}
	var problems []error
	for _, repository := range discovered {
		if existing, ok := byName[repository.Name]; ok {
			if existing.Project != repository.Project ||
				(existing.Branch != "" && existing.Branch != repository.Branch) ||
				(existing.GitHub != "" && repository.GitHub != "" && existing.GitHub != repository.GitHub) {
				problems = append(problems, fmt.Errorf(
					"live worktree %q conflicts with its portal registration", repository.Name,
				))
			}
			continue
		}
		result = append(result, repository)
		byName[repository.Name] = repository
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, errors.Join(problems...)
}

// DiscoverActiveRepositories scans each canonical Git source once and groups
// verified initiative worktrees by slug. Callers rendering an index can merge
// the result into every active manifest without an O(sessions * repositories)
// subprocess fanout.
func DiscoverActiveRepositories(workspace string) (map[string][]Repository, error) {
	result := make(map[string][]Repository)
	var problems []error
	for _, source := range repositorySources(workspace) {
		discovered, err := discoverSourceWorktrees(workspace, source)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for slug, repositories := range discovered {
			result[slug] = append(result[slug], repositories...)
		}
	}
	return result, errors.Join(problems...)
}

type repositorySource struct {
	project string
	path    string
	bare    bool
}

func repositorySources(workspace string) []repositorySource {
	var sources []repositorySource
	if info, err := os.Lstat(filepath.Join(workspace, ".git")); err == nil && info.Mode()&os.ModeSymlink == 0 {
		sources = append(sources, repositorySource{project: "workspace", path: workspace})
	}
	root := filepath.Join(workspace, "repos")
	entries, err := os.ReadDir(root)
	if err != nil {
		return sources
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".git") || !ValidSlug(strings.TrimSuffix(name, ".git")) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		sources = append(sources, repositorySource{
			project: strings.TrimSuffix(name, ".git"),
			path:    filepath.Join(root, name),
			bare:    true,
		})
	}
	return sources
}

func discoverSourceWorktrees(workspace string, source repositorySource) (map[string][]Repository, error) {
	args := source.gitArgs("worktree", "list", "--porcelain", "-z")
	output, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("inspect %s worktrees: %w", source.project, err)
	}
	remote, _ := gitText(source, "remote", "get-url", "origin")
	github := githubRepository(remote)
	defaultBranch, _ := gitText(source, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	defaultBranch = strings.TrimPrefix(defaultBranch, "origin/")
	if defaultBranch == "" && source.bare {
		defaultBranch, _ = gitText(source, "symbolic-ref", "--quiet", "--short", "HEAD")
	}
	if defaultBranch == "" {
		defaultBranch = "master"
	}

	root := filepath.Join(workspace, "worktrees")
	repositories := make(map[string][]Repository)
	for _, record := range bytes.Split(output, []byte{0, 0}) {
		fields := bytes.Split(record, []byte{0})
		values := make(map[string]string)
		for _, field := range fields {
			key, value, found := strings.Cut(string(field), " ")
			if found {
				values[key] = value
			}
		}
		path := values["worktree"]
		branch := strings.TrimPrefix(values["branch"], "refs/heads/")
		if path == "" || branch == "" {
			continue
		}
		group := filepath.Dir(path)
		slug := filepath.Base(group)
		name := filepath.Base(path)
		if !ValidSlug(slug) || !ValidSlug(name) || filepath.Dir(group) != root {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		realPath, pathErr := filepath.EvalSymlinks(path)
		realGroup, groupErr := filepath.EvalSymlinks(group)
		if pathErr != nil || groupErr != nil || filepath.Dir(realPath) != realGroup {
			continue
		}
		repositories[slug] = append(repositories[slug], Repository{
			Name: name, Project: source.project, GitHub: github,
			Branch: branch, DefaultBranch: defaultBranch,
		})
	}
	return repositories, nil
}

func (source repositorySource) gitArgs(args ...string) []string {
	if source.bare {
		return append([]string{"--git-dir=" + source.path}, args...)
	}
	return append([]string{"-C", source.path}, args...)
}

func gitText(source repositorySource, args ...string) (string, error) {
	output, err := exec.Command("git", source.gitArgs(args...)...).Output()
	return strings.TrimSpace(string(output)), err
}

func githubRepository(remote string) string {
	prefixes := []string{"git@github.com:", "ssh://git@github.com/", "https://github.com/", "http://github.com/"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(remote, prefix) {
			value := strings.TrimSuffix(strings.TrimPrefix(remote, prefix), ".git")
			parts := strings.Split(value, "/")
			if len(parts) == 2 && ValidSlug(parts[0]) && ValidSlug(parts[1]) {
				return value
			}
		}
	}
	return ""
}
