package repository

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

// SourceKind identifies whether the displayed bytes came from a live worktree,
// an archived final commit, a shared workspace, or a published tracking artifact.
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

// Session files retain their registration and artifact publication checks even
// when they are tracked by the workspace repository.
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

func (r ReviewReader) WorkspaceSource(ctx context.Context, path string) (SourceFile, error) {
	result := SourceFile{Path: path, Source: SourceWorkspace}
	if !ValidWorkspaceSourcePath(path) {
		return result, errors.New("invalid shared workspace path")
	}
	root, err := r.git(ctx, r.Workspace, 8192, "rev-parse", "--show-toplevel")
	if err != nil {
		return result, err
	}
	if strings.TrimSuffix(string(root), "\n") != r.Workspace {
		return result, errors.New("configured workspace is not the repository root")
	}
	result.Content, err = r.trackedSource(ctx, "", path)
	return result, err
}

// Source revalidates repository ownership on each request. Browser input selects
// only a literal path; the session registration supplies the repository and ref.
func (r ReviewReader) Source(ctx context.Context, slug string, item session.Repository, archived bool, path string) (SourceFile, error) {
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
		result.Content, err = r.blob(ctx, repo.Directory, fields[2], fields[0])
		return result, err
	}

	result.Content, err = r.trackedSource(ctx, filepath.Join("worktrees", slug, item.Name), path)
	return result, err
}

func (r ReviewReader) trackedSource(ctx context.Context, directory, path string) (ReviewBlob, error) {
	out, err := r.git(ctx, filepath.Join(r.Workspace, directory), 64*1024, "--literal-pathspecs", "ls-files", "--stage", "-z", "--", path)
	if err != nil {
		return ReviewBlob{}, err
	}
	if len(out) == 0 {
		return ReviewBlob{}, errors.New("file is not tracked")
	}
	for _, entry := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		metadata, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || name != path || len(fields) != 3 || !regularGitMode(fields[0]) {
			return ReviewBlob{}, errors.New("file is not a tracked regular file")
		}
	}
	file, info, err := session.OpenRegularFile(r.Workspace, filepath.Join(directory, path))
	if err != nil {
		return ReviewBlob{}, err
	}
	defer file.Close()
	return ReadSourcePreview(file, info.Size())
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
