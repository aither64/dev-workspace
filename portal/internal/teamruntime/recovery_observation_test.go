package teamruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

type interruptedObservationClient struct {
	*semanticTestClient
	stage       string
	failure     error
	rootReads   int
	memberReads map[string]int
}

func (c *interruptedObservationClient) ProveArchivedRootThread(ctx context.Context, id, cwd string) (workspacecodex.ArchiveState, error) {
	c.rootReads++
	if c.stage == "root recheck" && c.rootReads == 2 {
		return workspacecodex.ArchiveUnknown, c.failure
	}
	return c.semanticTestClient.ProveArchivedRootThread(ctx, id, cwd)
}

func (c *interruptedObservationClient) ProveArchivedThread(ctx context.Context, id, cwd, project string) (workspacecodex.ArchiveState, error) {
	c.memberReads[id]++
	if c.stage == "member recheck" && c.memberReads[id] == 2 {
		return workspacecodex.ArchiveUnknown, c.failure
	}
	return c.testClient.ProveArchivedThread(ctx, id, cwd, project)
}

func (c *interruptedObservationClient) HeadlessThreadMaterialized(ctx context.Context, id, cwd, project string) (bool, error) {
	if c.stage == "materialization" {
		return false, c.failure
	}
	return c.testClient.HeadlessThreadMaterialized(ctx, id, cwd, project)
}

func (c *interruptedObservationClient) ProveMaterializedActiveThread(ctx context.Context, id, cwd string) (workspacecodex.ArchiveState, error) {
	if c.stage == "legacy materialization" {
		return workspacecodex.ArchiveUnknown, c.failure
	}
	return c.testClient.ProveMaterializedActiveThread(ctx, id, cwd)
}

func (c *interruptedObservationClient) LoadedThreadIDs(ctx context.Context) ([]string, error) {
	if c.stage == "loaded discovery" {
		return nil, c.failure
	}
	return c.testClient.LoadedThreadIDs(ctx)
}

func (c *interruptedObservationClient) ReadThreadMetadata(ctx context.Context, id string, exclude bool) (codex.ThreadMetadata, error) {
	if c.stage == "loaded metadata" {
		return codex.ThreadMetadata{}, c.failure
	}
	return c.testClient.ReadThreadMetadata(ctx, id, exclude)
}

func TestRetainedRecoveryObservationPreservesTransportAtEveryProofBoundary(t *testing.T) {
	for _, stage := range []string{"root recheck", "member recheck", "materialization", "legacy materialization", "loaded discovery", "loaded metadata"} {
		for _, temporary := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "/refusal", true: "/transport"}[temporary], func(t *testing.T) {
				service, base, members := archivePreflightFixture(t, false)
				failure := error(errors.New("native refusal"))
				if temporary {
					failure = &codex.TransportError{Err: context.DeadlineExceeded}
				}
				client := &interruptedObservationClient{
					semanticTestClient: &semanticTestClient{testClient: base, tokens: map[string]string{"root-one": "root", members[0].Thread: "one", members[1].Thread: "two"}},
					stage:              stage, failure: failure, memberReads: map[string]int{},
				}
				service.Client = client
				if stage == "legacy materialization" {
					if _, err := service.Store.Update(context.Background(), "one", "root-one", false, func(roster *Roster) error {
						roster.Members[1].ProjectID = ""
						roster.Members[1].CreateAttempted = false
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				result, err := service.ObserveRetained(context.Background(), "one", "root-one", "bound-target")
				if err != nil || result.ActivityKnown || result.Idle || len(result.Diagnostics) != 1 {
					t.Fatal("interrupted proof granted readiness", result, err)
				}
				if (result.Diagnostics[0].Code == "transport_unavailable") != temporary {
					t.Fatal("proof lost transport/refusal distinction", result.Diagnostics)
				}
				if len(base.resumes) != 0 || len(base.archives) != 0 {
					t.Fatal("failed observation mutated native state")
				}
			})
		}
	}
}
