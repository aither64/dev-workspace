package repository

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ProgressFiles reads net changes from the comparison base through the working
// tree, including untracked files. This is advisory evidence, not a frozen review
// snapshot: ordinary concurrent edits can make a check unavailable and be retried
// at the next scheduled check. It never writes Git objects or refreshes the index.
func (r ReviewReader) ProgressFiles(ctx context.Context, repo ReviewRepository, base string) ([]ReviewFile, error) {
	out, err := r.git(ctx, repo.Worktree, maxReviewOutput, "diff", "--raw", "--numstat", "-z", "--no-abbrev", "--no-ext-diff", "--no-textconv", "--no-renames", base, "--")
	if err != nil {
		return nil, err
	}
	files, err := parseReviewFiles(out)
	if err != nil {
		return nil, err
	}
	out, err = r.git(ctx, repo.Worktree, maxReviewOutput, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, name := range bytes.Split(out, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		if len(files) >= maxReviewFiles || ctx.Err() != nil {
			return nil, ErrReviewLimit
		}
		path := string(name)
		if !validReviewPath(path) {
			return nil, ErrWorktreeState
		}
		f := ReviewFile{Path: path, Status: "?"}
		absolute := filepath.Join(repo.Worktree, path)
		info, err := os.Lstat(absolute)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > MaxReviewBlobBytes {
			f.Limited = true
		} else {
			file, err := os.Open(absolute)
			if err != nil {
				return nil, err
			}
			content, err := io.ReadAll(io.LimitReader(file, MaxReviewBlobBytes+1))
			file.Close()
			if err != nil {
				return nil, err
			}
			if len(content) > MaxReviewBlobBytes {
				f.Limited = true
			} else if !bytes.ContainsRune(content, 0) {
				added := int64(bytes.Count(content, []byte{'\n'}))
				if len(content) > 0 && content[len(content)-1] != '\n' {
					added++
				}
				removed := int64(0)
				f.Additions, f.Deletions = &added, &removed
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// Categories are path hints, not proof of generated content or a scope limit.
// The observer must inspect unfamiliar large files before treating them as cheap.
func ProgressCategory(path string) string {
	name := strings.ToLower(filepath.Base(path))
	path = strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.HasSuffix(name, ".lock"), strings.HasSuffix(name, ".lockb"), name == "go.sum", name == "package-lock.json", name == "yarn.lock", name == "pnpm-lock.yaml":
		return "lock/dependency"
	case strings.HasSuffix(name, ".md"), strings.HasSuffix(name, ".rst"), strings.HasPrefix(path, "docs/"):
		return "documentation"
	case strings.Contains(path, "/generated/"), strings.HasPrefix(path, "generated/"), strings.HasSuffix(name, ".pb.go"), strings.HasSuffix(name, ".generated.ts"):
		return "possibly generated"
	default:
		return "implementation/other"
	}
}
