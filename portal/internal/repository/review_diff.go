package repository

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

const LargeReviewDiffLines = 2000

func (f ReviewFile) LargeDiff() bool {
	return f.Additions != nil && f.Deletions != nil && *f.Additions+*f.Deletions > LargeReviewDiffLines
}

// ReviewChange is a zero-based line range. Empty ranges point at an insertion
// boundary; line counts exclude the empty suffix after a final newline.
type ReviewChange struct {
	OldStart int `json:"oldStart"`
	OldLines int `json:"oldLines"`
	NewStart int `json:"newStart"`
	NewLines int `json:"newLines"`
}

type ReviewDiff struct {
	Changes []ReviewChange `json:"changes"`
}

var reviewHunk = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

func reviewLineCount(text string) int {
	count := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		count++
	}
	return count
}

func (r ReviewReader) fileDiff(ctx context.Context, repo ReviewRepository, file ReviewFile, before, after ReviewBlob) (*ReviewDiff, error) {
	diff := &ReviewDiff{Changes: []ReviewChange{}}
	oldLines, newLines := reviewLineCount(before.Text), reviewLineCount(after.Text)
	if before.Kind == "absent" || after.Kind == "absent" {
		if oldLines+newLines > 0 {
			diff.Changes = append(diff.Changes, ReviewChange{OldLines: oldLines, NewLines: newLines})
		}
	} else {
		// Both arguments are verified immutable blob IDs from the raw change
		// record. No pathspec, worktree content, filter or user revision is used.
		out, err := r.git(ctx, repo.Directory, maxReviewOutput, "diff", "--text", "--no-color", "--no-ext-diff", "--no-textconv", "--diff-algorithm=myers", "--indent-heuristic", "--unified=0", "--inter-hunk-context=0", file.OldObject, file.NewObject, "--")
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(out, []byte{'\n'}) {
			if !bytes.HasPrefix(line, []byte("@@ ")) {
				continue
			}
			parts := reviewHunk.FindSubmatch(line)
			if parts == nil {
				return nil, errors.New("unexpected Git hunk")
			}
			values := [4]int{}
			for i := range values {
				if (i == 1 || i == 3) && len(parts[i+1]) == 0 {
					values[i] = 1
					continue
				}
				value, err := strconv.Atoi(string(parts[i+1]))
				if err != nil || value > MaxReviewLines {
					return nil, errors.New("invalid Git hunk range")
				}
				values[i] = value
			}
			if values[1] > 0 {
				values[0]--
			}
			if values[3] > 0 {
				values[2]--
			}
			diff.Changes = append(diff.Changes, ReviewChange{values[0], values[1], values[2], values[3]})
		}
	}
	oldEnd, newEnd, added, deleted := 0, 0, 0, 0
	for _, change := range diff.Changes {
		if change.OldStart < oldEnd || change.NewStart < newEnd || change.OldStart-oldEnd != change.NewStart-newEnd ||
			change.OldStart+change.OldLines > oldLines || change.NewStart+change.NewLines > newLines {
			return nil, errors.New("Git hunks do not match the file contents")
		}
		oldEnd, newEnd = change.OldStart+change.OldLines, change.NewStart+change.NewLines
		added += change.NewLines
		deleted += change.OldLines
	}
	if oldLines-oldEnd != newLines-newEnd ||
		(file.Additions != nil && int64(added) != *file.Additions) || (file.Deletions != nil && int64(deleted) != *file.Deletions) {
		return nil, errors.New("Git hunks do not match the change statistics")
	}
	return diff, nil
}
