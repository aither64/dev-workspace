package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// SourceKind identifies whether the displayed bytes came from a live worktree,
// an archived final commit, a shared workspace, or a session tracking file.
type SourceKind string

const (
	SourceWorktree         SourceKind = "worktree"
	SourceArchive          SourceKind = "archive"
	SourceArtifact         SourceKind = "artifact"
	SourceArchivedArtifact SourceKind = "archived-artifact"
	SourceWorkspace        SourceKind = "workspace"
)

type SourceFile struct {
	Path       string     `json:"path"`
	Repository string     `json:"repository,omitempty"`
	Source     SourceKind `json:"source"`
	Revision   string     `json:"revision,omitempty"`
	Content    ReviewBlob `json:"content"`
}

func ValidSourcePath(path string) bool {
	if path == "" || len(path) > 4096 || filepath.IsAbs(path) || filepath.Clean(path) != path ||
		strings.ContainsAny(path, "\\") || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

// Session files retain their session lookup and repository registration checks
// even when they are tracked by the workspace repository.
func ValidWorkspaceSourcePath(path string) bool {
	if !ValidSourcePath(path) {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	switch first {
	case "work", "archive", "worktrees", "repos":
		return false
	}
	return true
}

const MaxSourceDownloadBytes = 10 * 1024 * 1024

var ErrSourceDownloadLimit = errors.New("file exceeds the 10 MiB download limit")

func (r ReviewReader) WorkspaceSource(ctx context.Context, path string) (SourceFile, error) {
	return r.WorkspaceSourceContent(ctx, path, false)
}

// Download mode uses exactly the same eligibility and resolution as previews.
// Its Text contains the original bytes and must only be served as an attachment.
func (r ReviewReader) WorkspaceSourceContent(ctx context.Context, path string, download bool) (SourceFile, error) {
	result := SourceFile{Path: path, Source: SourceWorkspace}
	if !ValidWorkspaceSourcePath(path) {
		return result, errors.New("invalid shared workspace path")
	}
	if err := r.workspaceSourceRoot(ctx); err != nil {
		return result, err
	}
	var err error
	result.Content, err = r.workingSourceContent(ctx, "", path, download)
	return result, err
}

func (r ReviewReader) workspaceSourceRoot(ctx context.Context) error {
	root, err := r.git(ctx, r.Workspace, 8192, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(string(root), "\n") != r.Workspace {
		return errors.New("configured workspace is not the repository root")
	}
	return nil
}

// TrackingSource preserves explicit artifact access and also admits ordinary
// workspace files beneath the session's actual tracking root. The artifact
// source values are retained for existing viewer clients.
func (r ReviewReader) TrackingSource(ctx context.Context, summary *session.Summary, path string) (SourceFile, error) {
	return r.TrackingSourceContent(ctx, summary, path, false)
}

type TrackingFile struct {
	Path       string `json:"path"`
	Label      string `json:"label,omitempty"`
	Registered bool   `json:"registered"`
}

// TrackingFiles enumerates metadata only. Opening a row still revalidates through
// TrackingSourceContent; listing never publishes files into the artifact catalog.
func (r ReviewReader) TrackingFiles(ctx context.Context, summary *session.Summary) ([]TrackingFile, error) {
	if summary.Workspace != r.Workspace || !session.ValidSlug(summary.Slug) ||
		(summary.Root != "work" && summary.Root != "archive") {
		return nil, errors.New("invalid session tracking root")
	}
	files := make(map[string]TrackingFile)
	for _, item := range session.AvailableArtifacts(summary) {
		if !ValidSourcePath(item.Path) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, _, err := session.OpenRegularFile(r.Workspace, filepath.Join(summary.Root, summary.Slug, item.Path))
		if err != nil {
			continue
		}
		file.Close()
		files[item.Path] = TrackingFile{Path: item.Path, Label: item.Label,
			Registered: item.Path != "plan.md" && item.Path != "state.md"}
	}
	// Explicit publication works without a workspace Git repository, as does its
	// existing source reader. Ordinary discovery still requires a verified root.
	_, gitErr := os.Lstat(filepath.Join(r.Workspace, ".git"))
	if !errors.Is(gitErr, os.ErrNotExist) {
		if err := r.workspaceSourceRoot(ctx); err != nil {
			return nil, err
		}
		prefix := filepath.ToSlash(filepath.Join(summary.Root, summary.Slug)) + "/"
		tracked, err := r.git(ctx, r.Workspace, maxReviewOutput, "--literal-pathspecs", "ls-files", "--stage", "-z", "--", strings.TrimSuffix(prefix, "/"))
		if err != nil {
			return nil, err
		}
		candidates := make(map[string]struct{})
		for _, entry := range strings.Split(strings.TrimSuffix(string(tracked), "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			metadata, path, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(metadata)
			if !ok || len(fields) != 3 {
				return nil, errors.New("invalid tracked file listing")
			}
			if regularGitMode(fields[0]) {
				candidates[path] = struct{}{}
			}
		}
		args, err := r.sourceExcludesArgs(ctx, r.Workspace)
		if err != nil {
			return nil, err
		}
		args = append(args, "--literal-pathspecs", "ls-files", "--others", "--exclude-standard", "-z", "--", strings.TrimSuffix(prefix, "/"))
		untracked, err := r.git(ctx, r.Workspace, maxReviewOutput, args...)
		if err != nil {
			return nil, err
		}
		for _, path := range strings.Split(strings.TrimSuffix(string(untracked), "\x00"), "\x00") {
			if path != "" {
				candidates[path] = struct{}{}
			}
		}
		if len(tracked)+len(untracked) > maxReviewOutput || len(candidates) > maxReviewFiles {
			return nil, ErrReviewLimit
		}
		for path := range candidates {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			relative, ok := strings.CutPrefix(path, prefix)
			if !ok || !ValidSourcePath(relative) {
				continue
			}
			if _, found := files[relative]; found {
				continue
			}
			file, _, err := session.OpenRegularFile(r.Workspace, path)
			if err != nil {
				continue
			}
			file.Close()
			files[relative] = TrackingFile{Path: relative}
		}
	}
	if len(files) > maxReviewFiles {
		return nil, ErrReviewLimit
	}
	result := make([]TrackingFile, 0, len(files))
	for _, item := range files {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func (r ReviewReader) TrackingSourceContent(ctx context.Context, summary *session.Summary, path string, download bool) (SourceFile, error) {
	result := SourceFile{Path: path, Source: SourceArtifact}
	if summary.Archived {
		result.Source = SourceArchivedArtifact
	}
	file, info, err := session.OpenArtifact(summary, path, math.MaxInt64)
	if err == nil {
		defer file.Close()
		result.Path = session.ArtifactRelativePath(summary, path)
		result.Content, err = readSourceContent(file, info.Size(), download)
		return result, err
	}
	if summary.Workspace != r.Workspace || !session.ValidSlug(summary.Slug) ||
		(summary.Root != "work" && summary.Root != "archive") || !ValidSourcePath(path) {
		return result, errors.New("invalid session source path")
	}
	if err := r.workspaceSourceRoot(ctx); err != nil {
		return result, err
	}
	result.Content, err = r.workingSourceContent(ctx, "", filepath.Join(summary.Root, summary.Slug, path), download)
	return result, err
}

// Git source reads use the operator's effective excludes file without enabling
// unrelated operator configuration for repository commands.
func (r ReviewReader) sourceExcludesArgs(ctx context.Context, directory string) ([]string, error) {
	excludes, err := r.runGit(ctx, directory, 8192, true, "config", "--null", "--path", "--get", "core.excludesFile")
	if err == nil {
		if !strings.HasSuffix(string(excludes), "\x00") {
			return nil, errors.New("invalid Git excludes file setting")
		}
		return []string{"-c", "core.excludesFile=" + strings.TrimSuffix(string(excludes), "\x00")}, nil
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		return nil, err
	}
	return nil, nil
}

// Source revalidates repository ownership on each request. Browser input selects
// only a literal path; the session registration supplies the repository and ref.
func (r ReviewReader) Source(ctx context.Context, slug string, item session.Repository, archived bool, path string) (SourceFile, error) {
	return r.SourceContent(ctx, slug, item, archived, path, false)
}

func (r ReviewReader) SourceContent(ctx context.Context, slug string, item session.Repository, archived bool, path string, download bool) (SourceFile, error) {
	result := SourceFile{Path: path, Repository: item.Name, Source: SourceWorktree}
	if !ValidSourcePath(path) {
		return result, errors.New("invalid source path")
	}
	repo, err := r.Resolve(ctx, slug, item, archived)
	if err != nil {
		return result, err
	}
	if archived {
		result.Source, result.Revision = SourceArchive, repo.Head
		out, err := r.git(ctx, repo.Directory, 64*1024, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", repo.Head, "--", path)
		if err != nil {
			return result, err
		}
		entry := strings.TrimSuffix(string(out), "\x00")
		metadata, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || name != path || len(fields) != 3 || fields[1] != "blob" ||
			!regularGitMode(fields[0]) || !gitObjectPattern.MatchString(fields[2]) {
			return result, errors.New("file is absent from the recorded final commit")
		}
		if download {
			sizeBytes, sizeErr := r.git(ctx, repo.Directory, 1024, "cat-file", "-s", fields[2])
			if sizeErr != nil {
				return result, sizeErr
			}
			size, sizeErr := strconv.ParseInt(strings.TrimSpace(string(sizeBytes)), 10, 64)
			if sizeErr != nil || size < 0 {
				return result, errors.New("invalid Git blob size")
			}
			if size > MaxSourceDownloadBytes {
				return result, ErrSourceDownloadLimit
			}
			data, readErr := r.git(ctx, repo.Directory, MaxSourceDownloadBytes+1, "cat-file", "blob", fields[2])
			if readErr != nil {
				return result, readErr
			}
			result.Content, err = readSourceContent(bytes.NewReader(data), int64(len(data)), true)
		} else {
			result.Content, err = r.blob(ctx, repo.Directory, fields[2], fields[0])
		}
		return result, err
	}

	result.Content, err = r.workingSourceContent(ctx, filepath.Join("worktrees", slug, item.Name), path, download)
	return result, err
}

func (r ReviewReader) workingSourceContent(ctx context.Context, directory, path string, download bool) (ReviewBlob, error) {
	out, err := r.git(ctx, filepath.Join(r.Workspace, directory), 64*1024, "--literal-pathspecs", "ls-files", "--stage", "-z", "--", path)
	if err != nil {
		return ReviewBlob{}, err
	}
	if len(out) == 0 {
		// Read only the effective ignore-file setting with the operator's
		// configuration. Do not enable that configuration for repository reads.
		args, configErr := r.sourceExcludesArgs(ctx, filepath.Join(r.Workspace, directory))
		if configErr != nil {
			return ReviewBlob{}, configErr
		}
		args = append(args, "--literal-pathspecs", "ls-files", "--others", "--exclude-standard", "-z", "--", path)
		out, err = r.git(ctx, filepath.Join(r.Workspace, directory), 64*1024, args...)
		if err != nil {
			return ReviewBlob{}, err
		}
		// A literal directory pathspec may return children. Only the exact file
		// selected by the caller is eligible, never a prefix match.
		if string(out) != path+"\x00" {
			return ReviewBlob{}, errors.New("file is neither tracked nor non-ignored untracked")
		}
	} else {
		for _, entry := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
			metadata, name, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(metadata)
			if !ok || name != path || len(fields) != 3 || !regularGitMode(fields[0]) {
				return ReviewBlob{}, errors.New("file is not a tracked regular file")
			}
		}
	}
	file, info, err := session.OpenRegularFile(r.Workspace, filepath.Join(directory, path))
	if err != nil {
		return ReviewBlob{}, err
	}
	defer file.Close()
	return readSourceContent(file, info.Size(), download)
}

func regularGitMode(mode string) bool { return mode == "100644" || mode == "100755" }

// ReadSourcePreview applies the same byte and text limits as Git blob previews.
// The read remains bounded if a live file grows after its size was inspected.
func ReadSourcePreview(reader io.Reader, size int64) (ReviewBlob, error) {
	blob := ReviewBlob{Kind: "file", Bytes: size}
	if size > MaxReviewBlobBytes {
		blob.Limited = true
		return blob, nil
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxReviewBlobBytes+1))
	if err != nil {
		return blob, err
	}
	blob.Bytes = int64(len(data))
	if len(data) > MaxReviewBlobBytes {
		blob.Limited = true
		return blob, nil
	}
	return previewText(data, blob), nil
}

func readSourceContent(reader io.Reader, size int64, download bool) (ReviewBlob, error) {
	if !download {
		return ReadSourcePreview(reader, size)
	}
	if size > MaxSourceDownloadBytes {
		return ReviewBlob{}, ErrSourceDownloadLimit
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxSourceDownloadBytes+1))
	if err != nil {
		return ReviewBlob{}, err
	}
	if len(data) > MaxSourceDownloadBytes {
		return ReviewBlob{}, ErrSourceDownloadLimit
	}
	return ReviewBlob{Kind: "file", Bytes: int64(len(data)), Text: string(data)}, nil
}
