package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
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
	Path       string         `json:"path"`
	Repository string         `json:"repository,omitempty"`
	Source     SourceKind     `json:"source"`
	Revision   string         `json:"revision,omitempty"`
	Content    ReviewBlob     `json:"content"`
	Preview    *SourcePreview `json:"preview,omitempty"`
}

// SourcePreview is optional presentation metadata; Content retains original bytes.
type SourcePreview struct {
	Kind string `json:"kind"`
	HTML string `json:"html,omitempty"`
	URL  string `json:"url,omitempty"`
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

// TrackingSource reads only the selected session's current tracking root.
// Artifact source values remain compatible with existing viewer clients.
func (r ReviewReader) TrackingSource(ctx context.Context, summary *session.Summary, path string) (SourceFile, error) {
	return r.TrackingSourceContent(ctx, summary, path, false)
}

type TrackingFile struct {
	Path       string `json:"path"`
	Label      string `json:"label,omitempty"`
	Registered bool   `json:"registered"`
}

// TrackingFiles enumerates metadata only, independently of Git and registration.
// Every content request reopens the selected file through the same confinement.
func (r ReviewReader) TrackingFiles(ctx context.Context, summary *session.Summary) ([]TrackingFile, error) {
	if summary == nil || summary.Workspace != r.Workspace {
		return nil, errors.New("invalid session tracking root")
	}
	paths, err := session.ListTrackingFiles(ctx, summary, maxReviewFiles, maxReviewOutput)
	if errors.Is(err, session.ErrTrackingFilesLimit) {
		return nil, ErrReviewLimit
	}
	if err != nil {
		return nil, err
	}
	result := make([]TrackingFile, 0, len(paths))
	for _, path := range paths {
		result = append(result, TrackingFile{Path: path})
	}
	return result, nil
}

func (r ReviewReader) TrackingSourceContent(ctx context.Context, summary *session.Summary, path string, download bool) (SourceFile, error) {
	result := SourceFile{Path: path, Source: SourceArtifact}
	if summary != nil && summary.Archived {
		result.Source = SourceArchivedArtifact
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if summary == nil || summary.Workspace != r.Workspace {
		return result, errors.New("invalid session tracking root")
	}
	file, info, canonical, err := session.OpenTrackingFile(summary, path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	result.Path = canonical
	result.Content, err = readSourceContent(file, info.Size(), download)
	if err == nil {
		err = ctx.Err()
	}
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
