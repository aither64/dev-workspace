package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/repository"
	"golang.org/x/sys/unix"
)

func putTestWorktreeSnapshot(s *repositoryReviewService, snapshot *repositoryReviewSnapshot) error {
	reservation, err := s.reserveWorktree()
	if err != nil {
		return err
	}
	defer s.releaseWorktreeReservation(reservation)
	return s.putWorktree(snapshot, reservation)
}

func TestWorktreeReviewHTTPIsEphemeralAndKeepsCommittedLinks(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	query := "?repository=" + repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-"
	committed := reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
	review := reviewString(t, committed["review"])
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("middle\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runWebGit(t, "-C", worktree, "add", "file")
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("working\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "new"), []byte("untracked\n"), 0644); err != nil {
		t.Fatal(err)
	}
	staged := reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":"staged"}`, 200)
	unstaged := reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":"unstaged"}`, 200)
	if s.reviews().reservedWorktreeBytes != 0 {
		t.Fatal("successful capture kept its reservation")
	}
	if reviewString(t, staged["kind"]) != "staged" || reviewString(t, unstaged["kind"]) != "unstaged" ||
		string(staged["ephemeral"]) != "true" || len(staged["capturedAt"]) == 0 || reviewString(t, staged["review"]) != "" {
		t.Fatalf("ephemeral fields: staged=%v", staged)
	}
	var stagedFiles, unstagedFiles []repository.ReviewFile
	if err := json.Unmarshal(staged["files"], &stagedFiles); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(unstaged["files"], &unstagedFiles); err != nil {
		t.Fatal(err)
	}
	if len(stagedFiles) != 1 || stagedFiles[0].Path != "file" || len(unstagedFiles) != 2 {
		t.Fatalf("layer files: %#v / %#v", stagedFiles, unstagedFiles)
	}
	stagedID := reviewString(t, staged["snapshot"])
	unstagedID := reviewString(t, unstaged["snapshot"])
	file := reviewRequest(t, s, "GET", endpoint+"file"+query+"&snapshot="+unstagedID+"&file="+unstagedFiles[0].ID, "", 200)
	var content repository.ReviewContent
	encoded, _ := json.Marshal(file)
	if err := json.Unmarshal(encoded, &content); err != nil {
		t.Fatal(err)
	}
	if content.Before.Text != "middle\n" || content.After.Text != "working\n" {
		t.Fatalf("middle layer: %#v", content)
	}
	if err := os.WriteFile(filepath.Join(worktree, "file"), []byte("later\n"), 0644); err != nil {
		t.Fatal(err)
	}
	restored := reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&snapshot="+unstagedID, "", 200)
	if reviewString(t, restored["snapshot"]) != unstagedID {
		t.Fatal("snapshot URL recaptured")
	}
	file = reviewRequest(t, s, "GET", endpoint+"file"+query+"&snapshot="+unstagedID+"&file="+unstagedFiles[0].ID, "", 200)
	if !strings.Contains(string(file["after"]), "working") {
		t.Fatalf("snapshot followed worktree: %v", file)
	}
	reviewRequest(t, s, "POST", endpoint+"comparison"+query, fmt.Sprintf(`{"kind":"staged","snapshot":%q}`, stagedID), 400)
	reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":"unknown"}`, 400)
	reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":null}`, 400)
	reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":"staged","path":"file"}`, 400)
	unauthorized := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthorized, httptest.NewRequest("POST", endpoint+"comparison"+query, strings.NewReader(`{"kind":"staged"}`)))
	if unauthorized.Code != 403 {
		t.Fatalf("capture without origin = %d", unauthorized.Code)
	}
	reviewRequest(t, s, "GET", "/api/sessions/other/repository-comparison"+query+"&snapshot="+unstagedID, "", 409)
	committedAgain := reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&review="+review, "", 200)
	if reviewString(t, committedAgain["review"]) != review {
		t.Fatal("durable review changed")
	}
	delete(s.reviews().snapshots, unstagedID)
	reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&snapshot="+unstagedID, "", 409)
	// Registration changes must invalidate a worktree token, even when its
	// process-owned bytes still exist.
	manifest := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(strings.Replace(string(data), "branch: feature", "branch: other", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&snapshot="+stagedID, "", 409)
}

func TestWorktreeSnapshotStoreEvictsOnlyIdleReaders(t *testing.T) {
	s := &repositoryReviewService{snapshots: make(map[string]*repositoryReviewSnapshot)}
	for i := 0; i < 32; i++ {
		capture := &repository.WorktreeCapture{RawBytes: 1}
		if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: capture}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.snapshots) != 32 || s.worktreeBytes != 32 {
		t.Fatalf("store bounds %d/%d", len(s.snapshots), s.worktreeBytes)
	}
	if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: 1}}); err == nil {
		t.Fatal("evicted active reader")
	}
	for _, item := range s.snapshots {
		s.releaseWorktree(item)
		break
	}
	if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: 1}}); err != nil {
		t.Fatal(err)
	}
	if len(s.snapshots) != 32 || s.worktreeBytes != 32 {
		t.Fatalf("store after eviction %d/%d", len(s.snapshots), s.worktreeBytes)
	}
}

func TestWorktreeSnapshotStoreEnforcesRawByteBudget(t *testing.T) {
	s := &repositoryReviewService{snapshots: make(map[string]*repositoryReviewSnapshot)}
	if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: repository.MaxWorktreeSnapshotBytes + 1}}); err == nil {
		t.Fatal("oversized capture admitted")
	}
	for i := 0; i < 4; i++ {
		if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: repository.MaxWorktreeSnapshotBytes}}); err != nil {
			t.Fatal(err)
		}
	}
	if s.worktreeBytes != repository.MaxWorktreeStoreBytes {
		t.Fatalf("total raw bytes = %d", s.worktreeBytes)
	}
	if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: 1}}); err == nil {
		t.Fatal("evicted leased bytes")
	}
	for _, item := range s.snapshots {
		s.releaseWorktree(item)
		break
	}
	if err := putTestWorktreeSnapshot(s, &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: 1}}); err != nil {
		t.Fatal(err)
	}
	if s.worktreeBytes > repository.MaxWorktreeStoreBytes {
		t.Fatal("process raw byte budget exceeded")
	}
}

func TestWorktreeSnapshotReservationsBoundConcurrentCaptures(t *testing.T) {
	s := &repositoryReviewService{snapshots: make(map[string]*repositoryReviewSnapshot)}
	type result struct {
		reservation *worktreeReservation
		err         error
	}
	start := make(chan struct{})
	outcomes := make(chan result, 5)
	for i := 0; i < 5; i++ {
		go func() {
			<-start
			reservation, err := s.reserveWorktree()
			outcomes <- result{reservation, err}
		}()
	}
	close(start)
	var held []*worktreeReservation
	var refused int
	for i := 0; i < 5; i++ {
		outcome := <-outcomes
		if outcome.err == nil {
			held = append(held, outcome.reservation)
		} else if outcome.err == repository.ErrReviewLimit {
			refused++
		} else {
			t.Fatal(outcome.err)
		}
	}
	if len(held) != 4 || refused != 1 || s.reservedWorktreeBytes != repository.MaxWorktreeStoreBytes {
		t.Fatalf("concurrent reservations: held=%d refused=%d bytes=%d", len(held), refused, s.reservedWorktreeBytes)
	}
	for _, reservation := range held {
		s.releaseWorktreeReservation(reservation)
	}
	if s.reservedWorktreeBytes != 0 {
		t.Fatal("released captures retained capacity")
	}
}

func TestWorktreeSnapshotReservationEvictsOnlyIdleAndConverts(t *testing.T) {
	s := &repositoryReviewService{snapshots: make(map[string]*repositoryReviewSnapshot)}
	var snapshots []*repositoryReviewSnapshot
	for i := 0; i < 4; i++ {
		item := &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: repository.MaxWorktreeSnapshotBytes}}
		if err := putTestWorktreeSnapshot(s, item); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, item)
	}
	if _, err := s.reserveWorktree(); err != repository.ErrReviewLimit {
		t.Fatalf("leased snapshots were evicted for a reservation: %v", err)
	}
	s.releaseWorktree(snapshots[0])
	reservation, err := s.reserveWorktree()
	if err != nil {
		t.Fatal(err)
	}
	if s.snapshots[snapshots[0].ID] != nil || s.worktreeBytes != 3*repository.MaxWorktreeSnapshotBytes || s.reservedWorktreeBytes != repository.MaxWorktreeSnapshotBytes {
		t.Fatal("reservation did not evict only the idle snapshot")
	}
	item := &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: 1}}
	if err := s.putWorktree(item, reservation); err != nil {
		t.Fatal(err)
	}
	s.releaseWorktreeReservation(reservation) // A successful conversion makes this a no-op.
	if s.reservedWorktreeBytes != 0 || s.worktreeBytes != 3*repository.MaxWorktreeSnapshotBytes+1 || item.Readers != 1 || !s.leaseWorktree(item) {
		t.Fatal("reservation was not converted to a leased stored snapshot")
	}
	s.releaseWorktree(item)
	s.releaseWorktree(item)
}

func TestWorktreeSnapshotReservationReleasesAfterPublicationError(t *testing.T) {
	s := &repositoryReviewService{snapshots: make(map[string]*repositoryReviewSnapshot)}
	reservation, err := s.reserveWorktree()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		id := fmt.Sprint(i)
		s.snapshots[id] = &repositoryReviewSnapshot{ID: id, Readers: 1}
	}
	item := &repositoryReviewSnapshot{Worktree: &repository.WorktreeCapture{RawBytes: 1}}
	if err := s.putWorktree(item, reservation); err != repository.ErrReviewLimit {
		t.Fatalf("publication evicted leased snapshots: %v", err)
	}
	s.releaseWorktreeReservation(reservation)
	if s.reservedWorktreeBytes != 0 || s.worktreeBytes != 0 || len(s.snapshots) != 32 {
		t.Fatal("failed publication leaked capacity or changed snapshots")
	}
	if err := s.putWorktree(item, nil); err != repository.ErrReviewLimit {
		t.Fatalf("unreserved publication succeeded: %v", err)
	}
}

func TestWorktreeSnapshotReservationReleasesOnCaptureError(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	path := filepath.Join(worktree, "file")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	query := "?repository=" + repository.ReviewID("project")
	reviewRequest(t, s, "POST", "/api/sessions/example/repository-comparison"+query, `{"kind":"unstaged"}`, 422)
	if s.reviews().reservedWorktreeBytes != 0 || s.reviews().worktreeBytes != 0 {
		t.Fatal("failed capture retained reservation or snapshot bytes")
	}
}

func TestWorktreeSubmoduleNoticeIsSnapshotMetadata(t *testing.T) {
	s, _, worktree, _ := reviewWebFixture(t)
	head := strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD"))
	runWebGit(t, "-C", worktree, "update-index", "--add", "--cacheinfo", "160000,"+head+",module")
	if err := os.Mkdir(filepath.Join(worktree, "module"), 0755); err != nil {
		t.Fatal(err)
	}
	query := "?repository=" + repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-comparison" + query
	payload := reviewRequest(t, s, "POST", endpoint, `{"kind":"unstaged"}`, 200)
	var submodules []repository.WorktreeSubmodule
	if err := json.Unmarshal(payload["unverifiedSubmodules"], &submodules); err != nil {
		t.Fatal(err)
	}
	var stats repository.ReviewStats
	if err := json.Unmarshal(payload["stats"], &stats); err != nil {
		t.Fatal(err)
	}
	if len(submodules) != 1 || submodules[0].Path != "module" || submodules[0].Object != head || stats.Files != 0 {
		t.Fatalf("unverified metadata/count: %#v %#v", submodules, stats)
	}
	token := reviewString(t, payload["snapshot"])
	runWebGit(t, "-C", worktree, "update-index", "--force-remove", "module")
	restored := reviewRequest(t, s, "GET", endpoint+"&snapshot="+token, "", 200)
	if string(restored["unverifiedSubmodules"]) != string(payload["unverifiedSubmodules"]) {
		t.Fatal("submodule metadata followed index")
	}
	reviewRequest(t, s, "GET", "/api/sessions/other/repository-comparison"+query+"&snapshot="+token, "", 409)
}

func TestArchivedReviewRejectsWorkingSnapshotsButKeepsCommits(t *testing.T) {
	s, _, worktree, base := reviewWebFixture(t)
	query := "?repository=" + repository.ReviewID("project")
	endpoint := "/api/sessions/example/repository-"
	working := reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":"unstaged"}`, 200)
	source := filepath.Join(s.config.Workspace, "work", "example")
	target := filepath.Join(s.config.Workspace, "archive", "example")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("schema: 1\nslug: example\nfinalized_at: '2026-09-30T12:00:00Z'\nrepositories:\n  - name: project\n    project: project\n    branch: feature\n    default_branch: master\n    initial_base_sha: %s\n    final_head_sha: %s\n", base, strings.TrimSpace(webGitOutput(t, "-C", worktree, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(target, "portal.yml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	writeWebTrackingFiles(t, target, "complete")
	reviewRequest(t, s, "GET", endpoint+"comparison"+query+"&snapshot="+reviewString(t, working["snapshot"]), "", 409)
	reviewRequest(t, s, "POST", endpoint+"comparison"+query, `{"kind":"staged"}`, 409)
	reviewRequest(t, s, "GET", endpoint+"history"+query, "", 200)
}
