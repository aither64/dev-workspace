package workspacecodex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/turnretry"
)

type retryBackend struct{ client *Client }

func (c *Client) RetryBackend() turnretry.Backend { return retryBackend{c} }

func (b retryBackend) Observe(ctx context.Context, scope turnretry.Scope) (turnretry.Observation, error) {
	s, err := b.client.Client.ReadTurnState(ctx, scope.Thread)
	if err != nil {
		return turnretry.Observation{}, err
	}
	o := turnretry.Observation{Turn: s.LatestTurnID, Status: s.LatestTurnStatus}
	if s.TerminalFailure != nil {
		o.Code = s.TerminalFailure.Code
		if o.Code == "other" && s.TerminalFailure.Message == "stream disconnected before completion: Codex is at capacity, please try again later" {
			o.Code = "serverOverloaded"
		}
	}
	return o, nil
}
func (b retryBackend) Boundary(_ context.Context, scope turnretry.Scope) (string, error) {
	return session.ObservationIdentity(scope.Workspace, scope.Slug, filepath.Join(scope.Workspace, "work", scope.Slug), scope.Root)
}

func (b retryBackend) Prove(ctx context.Context, scope turnretry.Scope, state *turnretry.State, turn string) error {
	if state.Handled == turn {
		return turnretry.ErrMissingProof
	}
	// A complete predecessor executed proof retains its original recovery path.
	if legacyRetryProof(state, turn) {
		return nil
	}
	if _, err := b.client.advanceRetryPrefix(ctx, state, turn); err != nil {
		b.client.forgetRetryReader(scope.Thread)
		return err
	}
	m, binding := state.ManagedTurn, state.Binding
	if m == nil || m.Turn != turn || m.Count == 0 || binding == nil || m.Epoch != state.Epoch || m.BindingDigest != binding.Digest {
		return turnretry.ErrMissingProof
	}
	native, err := b.client.Client.ReadTurnContext(ctx, scope.Thread, turn)
	if err != nil {
		return err
	}
	if native.ThreadID != scope.Thread || native.TurnID != turn || native.Cwd != filepath.Join(scope.Workspace, "work", scope.Slug) {
		return turnretry.ErrMissingProof
	}
	contributor := turnretry.Snapshot{Epoch: state.Epoch, Version: 1, ID: "managed:" + turn, Turn: turn,
		Provider: binding.Provider, CommonInstructions: binding.CommonInstructions,
		SocketIdentity: binding.SocketIdentity, BindingDigest: binding.Digest, TextDigest: m.Digest}
	contributor.Options.ThreadPolicy = binding.Policy
	contributor.Options.AdditionalContext = m.AdditionalContext
	proof, err := proveContributors(scope, binding, native, []turnretry.Snapshot{contributor})
	if err != nil {
		return err
	}
	if err := state.RetainProof(proof); err != nil {
		return err
	}
	return nil
}

func proveContributors(scope turnretry.Scope, binding *turnretry.Binding, native codex.TurnContext, contributors []turnretry.Snapshot) (turnretry.Snapshot, error) {
	if binding == nil || binding.Epoch == 0 || native.ThreadID != scope.Thread || native.TurnID == "" || native.Cwd != filepath.Join(scope.Workspace, "work", scope.Slug) {
		return turnretry.Snapshot{}, turnretry.ErrMissingProof
	}
	proof := contributors[0]
	context := map[string]codex.AdditionalContextEntry{}
	ids := []string{}
	for _, p := range contributors {
		if p.Epoch != binding.Epoch || p.BindingDigest != binding.Digest || p.SocketIdentity != binding.SocketIdentity || p.Provider != binding.Provider || p.CommonInstructions != binding.CommonInstructions || !reflect.DeepEqual(p.Options.ThreadPolicy, binding.Policy) {
			return turnretry.Snapshot{}, turnretry.ErrMissingProof
		}
		if p.Origin == "queue" && len(p.Options.AdditionalContext) != 0 {
			return turnretry.Snapshot{}, turnretry.ErrMissingProof
		}
		for key, value := range p.Options.AdditionalContext {
			if old, ok := context[key]; ok && old != value {
				return turnretry.Snapshot{}, turnretry.ErrMissingProof
			}
			context[key] = value
		}
		ids = append(ids, p.ID+":"+p.TextDigest)
	}
	if err := authorizedNativePolicy(native, binding.Policy); err != nil {
		return turnretry.Snapshot{}, err
	}
	sort.Strings(ids)
	proof.ID, proof.Turn, proof.Origin, proof.Proven = "executed:"+native.TurnID, native.TurnID, "executed", true
	proof.TextDigest = textHash(strings.Join(ids, "\n"))
	proof.Options.Model, proof.Options.ReasoningEffort = native.Model, native.ReasoningEffort
	proof.Options.AdditionalContext = context
	proof.Mode = native.CollaborationMode
	data, err := json.Marshal(native)
	if err != nil {
		return turnretry.Snapshot{}, err
	}
	proof.NativeContext = data
	proof.Digest, err = proof.Hash()
	return proof, err
}

func authorizedNativePolicy(native codex.TurnContext, policy codex.ThreadPolicy) error {
	if policy.Sandbox != "" {
		var sandbox struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(native.SandboxPolicy, &sandbox) != nil {
			return turnretry.ErrMissingProof
		}
		// Native rollout policy uses kebab-case, unlike v2 request enums.
		if sandbox.Type != policy.Sandbox {
			return turnretry.ErrMissingProof
		}
	}
	if policy.ApprovalPolicy != "" {
		var approval string
		if json.Unmarshal(native.ApprovalPolicy, &approval) != nil || approval != policy.ApprovalPolicy {
			return turnretry.ErrMissingProof
		}
	}
	return nil
}

func (b retryBackend) Check(ctx context.Context, scope turnretry.Scope, proof turnretry.Snapshot) error {
	state, err := b.client.RetryStore().Load(scope)
	if err != nil {
		return err
	}
	if state.Binding == nil || state.Epoch != proof.Epoch || state.Binding.Epoch != state.Epoch || state.Binding.Digest != proof.BindingDigest || !proof.Proven {
		return turnretry.ErrMissingProof
	}
	epoch, err := session.SocketIdentity(scope.Socket)
	if err != nil || epoch != proof.SocketIdentity || !b.client.allowImplicitResume(scope.Thread) {
		return errors.New("retry execution permission is held")
	}
	policy, err := b.client.RetryPolicyResolver(ctx, scope.Thread)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(policy, proof.Options.ThreadPolicy) || proof.CommonInstructions != b.client.retryCommonInstructions {
		return errors.New("retry authorized binding changed")
	}
	provider, err := b.client.retryProvider(ctx, scope)
	if err != nil || provider != proof.Provider {
		return errors.New("retry provider changed")
	}
	s, err := b.client.Client.ReadTurnState(ctx, scope.Thread)
	if err != nil {
		return err
	}
	if !s.IsLoadedFailedTurn(proof.Turn) {
		return errors.New("retry requires the exact latest failed turn on a loaded thread")
	}
	// Current managed mode/access changes cancel the chain under this same
	// admission fence. Exact failed-turn context plus the unchanged managed
	// binding is authority; the transcript display-mode cache is not.
	native, err := b.client.Client.ReadTurnContext(ctx, scope.Thread, proof.Turn)
	if err != nil {
		return err
	}
	data, err := json.Marshal(native)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(json.RawMessage(data), proof.NativeContext) {
		return turnretry.ErrMissingProof
	}
	return authorizedNativePolicy(native, policy)
}

func (b retryBackend) Reconcile(ctx context.Context, scope turnretry.Scope, p turnretry.Pending) (codex.SendReceipt, bool, error) {
	return b.client.Client.ReconcileSendWithOptions(ctx, scope.Thread, turnretry.Continuation, p.ID, p.Action, p.Snapshot.Options)
}
func (b retryBackend) Continue(ctx context.Context, scope turnretry.Scope, p turnretry.Pending) (codex.SendReceipt, error) {
	return b.client.Client.ContinueFailedTurn(ctx, scope.Thread, turnretry.Continuation, p.ID, p.Action, p.Failure, p.Snapshot.Options)
}
func (b retryBackend) NeverSubmitted(_ context.Context, scope turnretry.Scope, p turnretry.Pending) (bool, error) {
	return b.client.Client.DiscardPreparedSendWithOptions(scope.Thread, turnretry.Continuation, p.ID, p.Action, p.Snapshot.Options)
}
