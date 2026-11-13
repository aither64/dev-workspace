package piproviders

import (
	"sort"

	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// ProjectRuntimeAccounts checks exact frozen targets against the canonical
// configured store and native key. It never selects credentials by account label.
// Policy validation remains separate and must cover the complete retained chain.
func ProjectRuntimeAccounts(c Catalog, targets []Target, canonicalCredentialRef string) ([]piruntime.AccountBinding, error) {
	if err := ValidateCatalog(c); err != nil {
		return nil, err
	}
	if canonicalCredentialRef != piruntime.CanonicalCredentialRef || len(targets) == 0 || len(targets) > 4096 {
		return nil, ErrUnavailableRoute
	}
	bindings := map[string]string{}
	for _, target := range targets {
		provider, account, err := findTarget(c, target)
		if err != nil {
			return nil, ErrUnavailableRoute
		}
		if account.CredentialRef != canonicalCredentialRef || account.CredentialKey != provider.NativeID {
			return nil, ErrUnavailableRoute
		}
		if previous, ok := bindings[provider.NativeID]; ok && previous != account.Ref {
			return nil, ErrUnavailableRoute
		}
		bindings[provider.NativeID] = account.Ref
	}
	return sortedBindings(bindings)
}

// SelectableRuntimeAccounts projects all available canonical routes, including
// providers absent from initial primary selections. Unsupported/unverified rows
// are skipped without changing raw gates or the immutable catalog digest.
func SelectableRuntimeAccounts(c Catalog, canonicalCredentialRef string) ([]piruntime.AccountBinding, error) {
	if err := ValidateCatalog(c); err != nil {
		return nil, err
	}
	if canonicalCredentialRef != piruntime.CanonicalCredentialRef {
		return nil, ErrUnavailableRoute
	}
	providers := map[string]Provider{}
	accounts := map[string]Account{}
	for _, provider := range c.Providers {
		providers[provider.ID] = provider
	}
	for _, account := range c.Accounts {
		accounts[account.Ref] = account
	}
	bindings := map[string]string{}
	for _, route := range c.Routes {
		if route.State != "available" {
			continue
		}
		provider, account := providers[route.ProviderID], accounts[route.AccountRef]
		if account.CredentialRef != canonicalCredentialRef || account.CredentialKey != provider.NativeID {
			continue
		}
		if previous, ok := bindings[provider.NativeID]; ok && previous != account.Ref {
			return nil, ErrUnavailableRoute
		}
		bindings[provider.NativeID] = account.Ref
	}
	return sortedBindings(bindings)
}

func sortedBindings(bindings map[string]string) ([]piruntime.AccountBinding, error) {
	if len(bindings) == 0 || len(bindings) > 64 {
		return nil, ErrUnavailableRoute
	}
	providers := make([]string, 0, len(bindings))
	for provider := range bindings {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	out := make([]piruntime.AccountBinding, 0, len(providers))
	for _, provider := range providers {
		out = append(out, piruntime.AccountBinding{Provider: provider, AccountRef: bindings[provider]})
	}
	return out, nil
}
