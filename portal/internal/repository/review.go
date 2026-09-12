package repository

// The review reader never fetches, checks out files, runs filters, or accepts a
// browser-supplied path or revision. All reads use a verified canonical repository
// and immutable object IDs resolved from the session's registered feature branch.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aither64/dev-workspace/portal/internal/session"
)

const (
	ReviewPageSize     = 50
	MaxReviewBlobBytes = 512 * 1024
	MaxReviewLines     = 12000
	maxReviewOutput    = 4 * 1024 * 1024
	maxReviewFiles     = 5000
)

var ErrReviewLimit = errors.New("repository data exceeds the review limit")

type ReviewReader struct {
	Workspace string
	Timeout   time.Duration
	Jobs      chan struct{}
}
type ReviewRepository struct {
	Name, ID, GitHub, Directory, Head, DefaultRef, InitialBase string
	Archived                                                   bool
}
type ReviewPair struct {
	Base      string `json:"base"`
	Head      string `json:"head"`
	BaseLabel string `json:"baseLabel"`
	Warning   string `json:"warning,omitempty"`
}
type ReviewCommit struct {
	ID      string   `json:"id"`
	SHA     string   `json:"sha"`
	Subject string   `json:"subject"`
	Body    string   `json:"body,omitempty"`
	Message string   `json:"message"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	URL     string   `json:"url,omitempty"`
	Parents []string `json:"-"`
}
type ReviewHistory struct {
	Commits []ReviewCommit `json:"commits"`
	Page    int            `json:"page"`
	HasMore bool           `json:"hasMore"`
}
type ReviewFile struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	OldMode   string `json:"oldMode"`
	NewMode   string `json:"newMode"`
	OldObject string `json:"-"`
	NewObject string `json:"-"`
	Additions *int64 `json:"additions"`
	Deletions *int64 `json:"deletions"`
}
type ReviewStats struct {
	Files       int   `json:"files"`
	Additions   int64 `json:"additions"`
	Deletions   int64 `json:"deletions"`
	BinaryFiles int   `json:"binaryFiles"`
}

func FileStats(files []ReviewFile) ReviewStats {
	stats := ReviewStats{Files: len(files)}
	for _, file := range files {
		if file.Additions == nil || file.Deletions == nil {
			stats.BinaryFiles++
		} else {
			stats.Additions += *file.Additions
			stats.Deletions += *file.Deletions
		}
	}
	return stats
}

type ReviewBlob struct {
	Text           string `json:"text"`
	Bytes          int64  `json:"bytes"`
	Binary         bool   `json:"binary,omitempty"`
	Limited        bool   `json:"limited,omitempty"`
	MissingNewline bool   `json:"missingNewline,omitempty"`
	Kind           string `json:"kind"`
}
type ReviewContent struct {
	Before ReviewBlob `json:"before"`
	After  ReviewBlob `json:"after"`
}

func ReviewID(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

// limitedGitOutput cancels the process as soon as its output exceeds the cap;
// returning a short write alone can otherwise leave a child writing forever.
type limitedGitOutput struct {
	buffer   bytes.Buffer
	max      int
	cancel   context.CancelFunc
	exceeded bool
}

func (w *limitedGitOutput) Write(p []byte) (int, error) {
	if w.buffer.Len()+len(p) > w.max {
		w.exceeded = true
		w.cancel()
		return 0, ErrReviewLimit
	}
	return w.buffer.Write(p)
}
func (r ReviewReader) git(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if r.Jobs != nil {
		select {
		case r.Jobs <- struct{}{}:
			defer func() { <-r.Jobs }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	commandArgs := []string{"--no-pager", "--no-replace-objects", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "diff.external=", "-c", "core.quotePath=false", "-C", dir}
	cmd := exec.CommandContext(ctx, "git", append(commandArgs, args...)...)
	cmd.WaitDelay = 250 * time.Millisecond
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
	out := &limitedGitOutput{max: limit, cancel: cancel}
	stderr := &limitedGitOutput{max: 8192, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	if out.exceeded || stderr.exceeded {
		return nil, ErrReviewLimit
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("Git review timed out or was cancelled: %w", ctx.Err())
	}
	if err != nil {
		return nil, err
	}
	return out.buffer.Bytes(), nil
}

func (r ReviewReader) Resolve(ctx context.Context, slug string, item session.Repository, archived bool) (ReviewRepository, error) {
	result := ReviewRepository{Name: item.Name, ID: ReviewID(item.Name), GitHub: item.GitHub, InitialBase: item.InitialBaseSHA, Archived: archived}
	if !session.ValidSlug(slug) || !session.ValidSlug(item.Name) || !session.ValidSlug(item.Project) {
		return result, errors.New("invalid repository registration")
	}
	workspace, err := canonicalPath(r.Workspace)
	if err != nil {
		return result, err
	}
	common, err := (Runner{Workspace: workspace}).expectedCommonDir(item.Project)
	if err != nil {
		return result, err
	}
	result.Directory = common
	if item.DefaultBranch != "" {
		result.DefaultRef = "refs/remotes/origin/" + item.DefaultBranch
	}
	if archived {
		result.Head = item.FinalHeadSHA
	} else {
		worktree := filepath.Join(workspace, "worktrees", slug, item.Name)
		resolved, err := canonicalPath(worktree)
		if err != nil || resolved != worktree {
			return result, errors.New("registered worktree is unavailable")
		}
		out, err := r.git(ctx, worktree, 8192, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
		if err != nil {
			return result, errors.New("cannot inspect registered worktree")
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != 2 {
			return result, errors.New("unexpected worktree identity")
		}
		top, err := canonicalPath(lines[0])
		if err != nil || top != worktree {
			return result, errors.New("unexpected worktree root")
		}
		actualCommon, err := canonicalPath(lines[1])
		if err != nil || actualCommon != common {
			return result, errors.New("worktree belongs to another repository")
		}
		out, err = r.git(ctx, worktree, 8192, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(HEAD)", "--count=1", "--", "refs/heads/"+item.Branch)
		if err != nil {
			return result, errors.New("cannot resolve registered feature branch")
		}
		fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\x00")
		if len(fields) != 3 || fields[0] != "refs/heads/"+item.Branch || fields[2] != "*" {
			return result, errors.New("worktree is not on its registered feature branch")
		}
		result.Head = fields[1]
	}
	if !gitObjectPattern.MatchString(result.Head) {
		return result, errors.New("repository has no recorded reviewable head")
	}
	if _, err := r.commit(ctx, result.Directory, result.Head); err != nil {
		return result, errors.New("review head is unavailable locally")
	}
	return result, nil
}
func (r ReviewReader) commit(ctx context.Context, dir, ref string) (string, error) {
	out, err := r.git(ctx, dir, 1024, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !gitObjectPattern.MatchString(sha) {
		return "", errors.New("invalid Git commit identity")
	}
	return sha, nil
}
func (r ReviewReader) Pair(ctx context.Context, repo ReviewRepository, saved *ReviewPair) (ReviewPair, error) {
	pair := ReviewPair{Head: repo.Head}
	integrated := repo.Archived
	if !repo.Archived && repo.DefaultRef != "" {
		defaultHead, err := r.commit(ctx, repo.Directory, repo.DefaultRef)
		if err == nil {
			out, err := r.git(ctx, repo.Directory, 1024, "merge-base", defaultHead, repo.Head)
			if err != nil {
				return pair, errors.New("cannot resolve the local merge base")
			}
			base := strings.TrimSpace(string(out))
			if !gitObjectPattern.MatchString(base) {
				return pair, errors.New("invalid local merge base")
			}
			integrated = base == repo.Head
			if !integrated {
				pair.Base = base
				pair.BaseLabel = "Merge base with locally available " + strings.TrimPrefix(repo.DefaultRef, "refs/remotes/")
				return pair, nil
			}
		} else {
			pair.Warning = "The default branch is unavailable locally."
		}
	}
	if integrated && saved != nil && saved.Head == repo.Head {
		if _, err := r.commit(ctx, repo.Directory, saved.Base); err == nil {
			pair.Base = saved.Base
			pair.BaseLabel = "Last viewed comparison for this integrated head"
			if strings.Contains(saved.BaseLabel, "fallback") {
				pair.BaseLabel = "Last viewed comparison (original recorded base fallback)"
				pair.Warning = saved.Warning
			}
			return pair, nil
		}
		pair.Warning = "The last viewed comparison base is unavailable locally."
	}
	if !gitObjectPattern.MatchString(repo.InitialBase) {
		return pair, errors.New("repository has no recorded comparison base")
	}
	if _, err := r.commit(ctx, repo.Directory, repo.InitialBase); err != nil {
		return pair, errors.New("recorded comparison base is unavailable locally")
	}
	pair.Base = repo.InitialBase
	pair.BaseLabel = "Original recorded base (fallback)"
	if pair.Warning == "" {
		pair.Warning = "No saved comparison is available for this head."
	}
	pair.Warning += " The original base may include upstream commits after a rebase."
	return pair, nil
}
func (r ReviewReader) History(ctx context.Context, repo ReviewRepository, pair ReviewPair, page int) (ReviewHistory, error) {
	result := ReviewHistory{Page: page, Commits: []ReviewCommit{}}
	if page < 0 || page > 2000 {
		return result, errors.New("invalid history page")
	}
	out, err := r.git(ctx, repo.Directory, maxReviewOutput, "log", "-z", "--no-show-signature", "--format="+reviewCommitFormat, "--max-count=51", "--skip="+strconv.Itoa(page*ReviewPageSize), pair.Base+".."+pair.Head, "--")
	if err != nil {
		return result, err
	}
	if len(out) == 0 {
		return result, nil
	}
	result.Commits, err = parseReviewCommits(out, repo.GitHub)
	if err != nil {
		return result, err
	}
	if len(result.Commits) > ReviewPageSize {
		result.HasMore = true
		result.Commits = result.Commits[:ReviewPageSize]
	}
	return result, nil
}

const reviewCommitFormat = "%H%x00%P%x00%an%x00%aI%x00%s%x00%b%x00%B"

func parseReviewCommits(out []byte, github string) ([]ReviewCommit, error) {
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	if len(fields)%7 != 0 {
		return nil, errors.New("unexpected Git history format")
	}
	result := make([]ReviewCommit, 0, len(fields)/7)
	for i := 0; i < len(fields); i += 7 {
		sha := string(fields[i])
		if !gitObjectPattern.MatchString(sha) {
			return nil, errors.New("invalid history commit")
		}
		c := ReviewCommit{ID: ReviewID(sha), SHA: sha, Parents: strings.Fields(string(fields[i+1])), Author: string(fields[i+2]), Date: string(fields[i+3]), Subject: string(fields[i+4]), Body: strings.TrimSpace(string(fields[i+5])), Message: string(fields[i+6])}
		if validReviewGitHub(github) {
			c.URL = "https://github.com/" + github + "/commit/" + sha
		}
		result = append(result, c)
	}
	return result, nil
}

// Restore reads the saved objects from the canonical repository. An old shared
// view remains usable after its feature branch moves or its worktree is removed.
func (r ReviewReader) Restore(ctx context.Context, slug string, item session.Repository, pair ReviewPair) (ReviewRepository, error) {
	if !gitObjectPattern.MatchString(pair.Base) || !gitObjectPattern.MatchString(pair.Head) {
		return ReviewRepository{}, errors.New("invalid saved comparison")
	}
	item.FinalHeadSHA = pair.Head
	repo, err := r.Resolve(ctx, slug, item, true)
	if err != nil {
		return repo, err
	}
	if _, err = r.commit(ctx, repo.Directory, pair.Base); err != nil {
		return repo, errors.New("saved comparison base is unavailable locally")
	}
	return repo, nil
}

// ComparisonCommit accepts only a commit in the exact recorded branch range,
// including commits that were not on the first history page.
func (r ReviewReader) ComparisonCommit(ctx context.Context, repo ReviewRepository, pair ReviewPair, sha string) (ReviewCommit, error) {
	if !gitObjectPattern.MatchString(sha) {
		return ReviewCommit{}, errors.New("invalid comparison commit")
	}
	ancestor := func(head string) (bool, error) {
		_, err := r.git(ctx, repo.Directory, 1024, "merge-base", "--is-ancestor", sha, head)
		var failure *exec.ExitError
		if errors.As(err, &failure) && failure.ExitCode() == 1 {
			return false, nil
		}
		return err == nil, err
	}
	reachable, err := ancestor(pair.Head)
	if err != nil {
		return ReviewCommit{}, err
	}
	if !reachable {
		return ReviewCommit{}, errors.New("commit is outside this comparison")
	}
	inBase, err := ancestor(pair.Base)
	if err != nil {
		return ReviewCommit{}, err
	}
	if inBase {
		return ReviewCommit{}, errors.New("commit is outside this comparison")
	}
	out, err := r.git(ctx, repo.Directory, maxReviewOutput, "show", "-s", "-z", "--no-show-signature", "--format="+reviewCommitFormat, sha, "--")
	if err != nil {
		return ReviewCommit{}, err
	}
	commits, err := parseReviewCommits(out, repo.GitHub)
	if err != nil {
		return ReviewCommit{}, err
	}
	if len(commits) != 1 || commits[0].SHA != sha {
		return ReviewCommit{}, errors.New("unexpected comparison commit")
	}
	return commits[0], nil
}
func validReviewGitHub(value string) bool {
	owner, name, ok := strings.Cut(value, "/")
	return ok && githubPartPattern.MatchString(owner) && githubPartPattern.MatchString(name)
}

func (r ReviewReader) CommitPair(ctx context.Context, repo ReviewRepository, commit ReviewCommit) (ReviewPair, error) {
	pair := ReviewPair{Head: commit.SHA, BaseLabel: "Parent of commit " + commit.SHA[:10]}
	if len(commit.Parents) > 0 {
		pair.Base = commit.Parents[0]
		if len(commit.Parents) > 1 {
			pair.BaseLabel = "First parent of merge commit " + commit.SHA[:10]
		}
		return pair, nil
	}
	// Empty trees have a stable object ID for each repository's object format.
	out, err := r.git(ctx, repo.Directory, 1024, "hash-object", "-t", "tree", "--stdin")
	if err != nil {
		return pair, err
	}
	pair.Base = strings.TrimSpace(string(out))
	pair.BaseLabel = "Empty tree before the root commit"
	return pair, nil
}
func (r ReviewReader) Files(ctx context.Context, repo ReviewRepository, pair ReviewPair) ([]ReviewFile, error) {
	// Keep rename detection complete for a successful comparison. The Git
	// deadline bounds expensive comparisons instead of silently losing renames.
	out, err := r.git(ctx, repo.Directory, maxReviewOutput, "diff", "--raw", "--numstat", "-z", "--no-abbrev", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", "--find-renames=50%", "-l0", pair.Base, pair.Head, "--")
	if err != nil {
		return nil, err
	}
	return parseReviewFiles(out)
}

func parseReviewFiles(out []byte) ([]ReviewFile, error) {
	fields := bytes.Split(out, []byte{0})
	result := []ReviewFile{}
	i := 0
	for i < len(fields) && bytes.HasPrefix(fields[i], []byte{':'}) {
		header := strings.Fields(string(fields[i]))
		i++
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") || i >= len(fields) {
			return nil, errors.New("unexpected Git change record")
		}
		f := ReviewFile{OldMode: strings.TrimPrefix(header[0], ":"), NewMode: header[1], OldObject: header[2], NewObject: header[3], Status: header[4], Path: string(fields[i])}
		i++
		if strings.HasPrefix(f.Status, "R") || strings.HasPrefix(f.Status, "C") {
			if i >= len(fields) {
				return nil, errors.New("incomplete Git rename")
			}
			f.OldPath = f.Path
			f.Path = string(fields[i])
			i++
		}
		if !gitObjectPattern.MatchString(f.OldObject) || !gitObjectPattern.MatchString(f.NewObject) {
			return nil, errors.New("invalid Git blob identity")
		}
		f.ID = ReviewID(strconv.Itoa(len(result)) + "\x00" + f.Path)
		result = append(result, f)
		if len(result) > maxReviewFiles {
			return nil, ErrReviewLimit
		}
	}
	byPath := make(map[string]int, len(result))
	for index, file := range result {
		byPath[file.OldPath+"\x00"+file.Path] = index
	}
	seen := make(map[int]bool, len(result))
	for i < len(fields) && len(fields[i]) > 0 {
		stat := bytes.SplitN(fields[i], []byte{'\t'}, 3)
		i++
		if len(stat) != 3 {
			return nil, errors.New("unexpected Git line statistics")
		}
		oldPath, path := "", string(stat[2])
		if path == "" {
			if i+1 >= len(fields) {
				return nil, errors.New("incomplete Git rename statistics")
			}
			oldPath, path = string(fields[i]), string(fields[i+1])
			i += 2
		}
		index, ok := byPath[oldPath+"\x00"+path]
		if !ok || seen[index] {
			return nil, errors.New("Git line statistics do not match changed files")
		}
		seen[index] = true
		if string(stat[0]) == "-" && string(stat[1]) == "-" {
			continue
		}
		added, err := strconv.ParseInt(string(stat[0]), 10, 64)
		if err != nil || added < 0 {
			return nil, errors.New("invalid Git addition count")
		}
		removed, err := strconv.ParseInt(string(stat[1]), 10, 64)
		if err != nil || removed < 0 {
			return nil, errors.New("invalid Git deletion count")
		}
		result[index].Additions, result[index].Deletions = &added, &removed
	}
	if len(seen) != len(result) || i != len(fields)-1 || len(fields[i]) != 0 {
		return nil, errors.New("incomplete Git line statistics")
	}
	return result, nil
}
func (r ReviewReader) Content(ctx context.Context, repo ReviewRepository, file ReviewFile) (ReviewContent, error) {
	before, err := r.blob(ctx, repo.Directory, file.OldObject, file.OldMode)
	if err != nil {
		return ReviewContent{}, err
	}
	after, err := r.blob(ctx, repo.Directory, file.NewObject, file.NewMode)
	if err != nil {
		return ReviewContent{}, err
	}
	return ReviewContent{Before: before, After: after}, nil
}
func (r ReviewReader) blob(ctx context.Context, dir, object, mode string) (ReviewBlob, error) {
	b := ReviewBlob{Kind: "file"}
	if mode == "000000" {
		b.Kind = "absent"
		return b, nil
	}
	if mode == "160000" {
		b.Kind = "submodule"
		b.Text = object + "\n"
		return b, nil
	}
	if mode == "120000" {
		b.Kind = "symlink"
	}
	out, err := r.git(ctx, dir, 1024, "cat-file", "-s", object)
	if err != nil {
		return b, errors.New("file object is unavailable locally")
	}
	b.Bytes, err = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || b.Bytes < 0 {
		return b, errors.New("invalid Git blob size")
	}
	if b.Bytes > MaxReviewBlobBytes {
		b.Limited = true
		return b, nil
	}
	out, err = r.git(ctx, dir, MaxReviewBlobBytes, "cat-file", "blob", object)
	if err != nil {
		return b, err
	}
	if bytes.IndexByte(out, 0) >= 0 || !utf8.Valid(out) {
		b.Binary = true
		return b, nil
	}
	missingNewline := len(out) > 0 && out[len(out)-1] != '\n'
	lines := bytes.Count(out, []byte{'\n'})
	if missingNewline {
		lines++
	}
	if lines > MaxReviewLines {
		b.Limited = true
		return b, nil
	}
	b.Text = string(out)
	b.MissingNewline = missingNewline
	return b, nil
}
