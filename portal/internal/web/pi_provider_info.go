package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aither64/codex-web/conversation/backend"
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/piproviders"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

type piProviderRuntime interface {
	piruntime.WorkflowObserver
	ReadStart(context.Context, agentbackend.SessionRef) (piruntime.StartRequest, error)
}

func (a *piAdmittedConversation) ReadProvider(ctx context.Context, id backend.Identity) (result backend.ProviderInfo, err error) {
	err = a.read(ctx, id, func(backend.Operations) error {
		runtime, ok := a.server.config.PiCreation.Runtime.(piProviderRuntime)
		if !ok {
			return piruntime.ErrUnavailable
		}
		observed, e := runtime.ObserveWorkflow(ctx, a.handle)
		if e != nil || observed.Handle != a.handle || !observed.Loaded {
			return piruntime.ErrUnavailable
		}
		start, e := runtime.ReadStart(ctx, a.handle.Conversation.Session)
		if e != nil || start.Session != a.handle.Conversation.Session {
			return piruntime.ErrUnavailable
		}
		frozen, e := piproviders.LoadInstalled(start.AppStorePath)
		if e != nil {
			return piruntime.ErrUnavailable
		}
		result, e = a.server.piProviderInfo(ctx, id, observed.Settings, frozen, start.AppStorePath)
		return e
	})
	return
}

// The current worker's immutable catalog owns identity. The host catalog can
// supply a route label and metrics only after exact endpoint/account binding.
func (s *Server) piProviderInfo(ctx context.Context, id backend.Identity, settings piruntime.EffectiveSettings, frozen piproviders.PackageConfiguration, app string) (backend.ProviderInfo, error) {
	var provider piproviders.Provider
	var account piproviders.Account
	found := false
	for _, p := range frozen.Catalog.Providers {
		if p.NativeID != settings.Provider {
			continue
		}
		for _, a := range frozen.Catalog.Accounts {
			if a.ProviderID == p.ID && a.Ref == settings.AccountRef {
				if found {
					return backend.ProviderInfo{}, piruntime.ErrUnavailable
				}
				provider, account, found = p, a, true
			}
		}
	}
	if !found {
		return backend.ProviderInfo{}, piruntime.ErrUnavailable
	}
	result := backend.ProviderInfo{ThreadID: id.ConversationID, Generation: id.Generation,
		ProviderID: provider.ID, ProviderLabel: provider.Label, NativeProviderID: provider.NativeID,
		AccountRef: account.Ref, ObservedAt: time.Now().UTC(),
		Usage: backend.ProviderUsage{State: "unavailable", Reason: "route_unavailable"}}
	// This is the existing built-in Pi Responses adapter boundary, not a
	// shared-interface fallback to the globally selected Codex account.
	if provider.NativeID != "openai" || provider.RuntimeConfigRef != "builtin" ||
		account.CredentialRef != piruntime.CanonicalCredentialRef || account.CredentialKey != provider.NativeID || account.AuthKind != "api_key" {
		result.Usage.State, result.Usage.Reason = "unsupported", "unsupported_credentials"
		return result, nil
	}
	models, err := piruntime.LoadInstalledModelConfiguration(app)
	if err != nil || models.Path == "" {
		return result, nil
	}
	catalog, err := codexproviders.ReadCatalog(codexproviders.CatalogPath())
	if err != nil {
		return result, nil
	}
	var route codexproviders.Provider
	matches := 0
	for _, p := range catalog.Providers {
		if p.Kind == "codex-lb" && p.BaseURL == models.BaseURL {
			route, matches = p, matches+1
		}
	}
	if matches != 1 {
		return result, nil
	}
	// A route match identifies the provider, not the account. Keep the member's
	// saved account reference even when credentials or usage are unavailable.
	result.ProviderID, result.ProviderLabel = route.ID, route.Label
	result.Usage.Reason = "credential_binding_unavailable"
	key, err := readPiLiteralKey(s.config.PiCreation.AuthDirectory, account.CredentialKey)
	if err != nil {
		return result, nil
	}
	credential, err := codexproviders.CaptureCredential(route)
	if err != nil || !credential.MatchesLiteral(key) {
		return result, nil
	}
	// Compare before cache lookup, and use this same captured token for HTTP.
	limits, err := s.loadCapturedProviderLimits(ctx, catalog.Digest, route, credential)
	if err != nil {
		result.Usage.Reason = "usage_unavailable"
		return result, nil
	}
	result.Usage = scopedProviderUsage(limits)
	return result, nil
}

// Only literal api_key entries are supported. Never resolve commands, env
// substitutions, OAuth, alternate auth stores or per-agent overrides.
func readPiLiteralKey(directory, provider string) (string, error) {
	refuse := errors.New("provider credential binding unavailable")
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", refuse
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !providerInfoOwned(info) {
		return "", refuse
	}
	path := filepath.Join(directory, "auth.json")
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 || !providerInfoOwned(info) {
		return "", refuse
	}
	f, err := os.Open(path)
	if err != nil {
		return "", refuse
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 || !providerInfoOwned(info) {
		return "", refuse
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	var entries map[string]json.RawMessage
	if err != nil || session.DecodePiCreationJSON(data, &entries, 1<<20) != nil {
		return "", refuse
	}
	var selected struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if session.DecodePiCreationJSON(entries[provider], &selected, 16384) != nil || selected.Type != "api_key" ||
		selected.Key == "" || len(selected.Key) > 16384 || strings.TrimSpace(selected.Key) != selected.Key ||
		strings.HasPrefix(selected.Key, "!") || strings.ContainsAny(selected.Key, "$\x00\r\n") {
		return "", refuse
	}
	return selected.Key, nil
}

func providerInfoOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func scopedProviderUsage(snapshot codexLimitsSnapshot) backend.ProviderUsage {
	metrics := &backend.ProviderMetrics{}
	if credits := snapshot.Credits; credits != nil {
		metrics.Credits = &backend.ProviderCredits{HasCredits: credits.HasCredits, Unlimited: credits.Unlimited, Balance: credits.Balance}
	}
	for _, window := range snapshot.Windows {
		used := window.UsedPercent
		metrics.Windows = append(metrics.Windows, backend.ProviderWindow{UsedPercent: &used, WindowDurationMins: window.WindowDurationMins, ResetsAt: window.ResetsAt})
	}
	if m := snapshot.Metrics; m != nil {
		metrics.PoolRemaining, metrics.Requests, metrics.Tokens, metrics.CostUSD = m.PoolRemaining, m.Requests, m.Tokens, m.CostUSD
		convert := func(rows []codexproviders.Quota) []backend.ProviderQuota {
			var result []backend.ProviderQuota
			for _, q := range rows {
				result = append(result, backend.ProviderQuota{Type: q.Type, Window: q.Window, Model: q.Model, Maximum: q.Maximum, Used: q.Used, Remaining: q.Remaining, ResetAt: q.ResetAt, Source: q.Source})
			}
			return result
		}
		metrics.KeyQuotas, metrics.UpstreamQuotas = convert(m.KeyQuotas), convert(m.UpstreamQuotas)
	}
	return backend.ProviderUsage{State: "available", ObservedAt: &snapshot.UpdatedAt, Metrics: metrics}
}
