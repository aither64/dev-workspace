package teamruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/monitoroperation"
)

type semanticMonitorClient struct{ *semanticTestClient }

func (c *semanticMonitorClient) StartThreadWithSettings(ctx context.Context, cwd string, env map[string]string, settings codex.ThreadSettings) (string, error) {
	thread, err := c.testClient.StartThreadWithSettings(ctx, cwd, env, settings)
	for i := range c.threads {
		if c.threads[i].ID == thread {
			c.threads[i].Source = "vscode"
		}
	}
	c.tokens[thread] = "utility-token"
	return thread, err
}
func (c *semanticMonitorClient) SendWithOptions(ctx context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	receipt, err := c.testClient.SendWithOptions(ctx, thread, text, id, action, options)
	receipt.ClientUserMessageID = id
	return receipt, err
}
func (c *semanticMonitorClient) ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error) {
	return codex.SendReceipt{}, false, nil
}

func TestRetainedObservationIncludesUtilityWithoutRosterAdoption(t *testing.T) {
	ctx := context.Background()
	service, base, members := archivePreflightFixture(t, false)
	client := &semanticMonitorClient{&semanticTestClient{testClient: base, tokens: map[string]string{
		"root-one": "root-token", members[0].Thread: "archived-token", members[1].Thread: "member-token",
	}}}
	service.Client = client
	roster, err := service.Store.Load("one", "root-one")
	if err != nil {
		t.Fatal(err)
	}
	store, err := monitoroperation.NewStore(service.Store.stateRoot, service.Store.workspace)
	if err != nil {
		t.Fatal(err)
	}
	owner := monitoroperation.Owner{Slug: "one", RootThreadID: "root-one", Incarnation: "fixture", OriginTurnID: "origin",
		Socket: service.Store.workspace + "/native.sock", SocketEpoch: "epoch", RuntimeSchema: 1}
	monitor := monitoroperation.Service{Store: store, Client: client,
		ResolveLead: func(context.Context, string) (monitoroperation.Owner, error) { return owner, nil },
		Resolve:     func(string) (monitoroperation.Owner, error) { return owner, nil },
		LockSession: func(context.Context, string) (func(), error) { return func() {}, nil },
		Admit:       func(context.Context, monitoroperation.Owner) error { return nil },
		Activate:    func(context.Context, monitoroperation.Owner, string) error { return nil },
		Policy: func(context.Context, monitoroperation.Owner) (monitoroperation.UtilityPolicy, error) {
			return monitoroperation.UtilityPolicy{PackageRoot: service.Store.workspace, CatalogDigest: strings.Repeat("a", 64),
				NativeName: "fixture-watcher", Model: "gpt-6-luna", Effort: "low", Instructions: "Observe the exact run.",
				Sandbox: "danger-full-access", ApprovalPolicy: "never", MaxConcurrent: 1}, nil
		},
	}
	op, err := monitor.Start(ctx, owner.RootThreadID, monitoroperation.StartRequest{ID: strings.Repeat("1", 32), Brief: "Observe exact CI attempt",
		Source: monitoroperation.Source{Kind: "ci", Provider: "github", Repository: "owner/project", WorkflowID: "check", RunID: "42", RunAttempt: 1, Commit: "revision"}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := service.ObserveRetained(ctx, "one", "root-one", "fixture-target")
	if err != nil || !observation.ActivityKnown || observation.Idle || len(observation.Subjects) != 4 {
		t.Fatal("utility was hidden or external effects were treated as idle", observation, err)
	}
	utility := observation.Subjects[3]
	if utility.Address != "monitor:"+op.ID || utility.ThreadID != op.Launch.ThreadID || utility.ProjectID != op.Launch.ProjectID {
		t.Fatal("utility subject impersonated a roster member", utility)
	}
	if _, err := monitor.Report(ctx, op.Launch.ThreadID, monitoroperation.ReportRequest{Slot: "terminal", Outcome: "passed", Conclusion: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := service.RequireArchiveReadyAll(ctx, "one", "root-one"); err != nil {
		t.Fatal("exact utility was classified as an unmanaged conversation", err)
	}
	after, err := service.Store.Load("one", "root-one")
	if err != nil || len(after.Members) != len(roster.Members) || after.Revision != roster.Revision {
		t.Fatal("monitor inventory changed roster", after, err)
	}
}
