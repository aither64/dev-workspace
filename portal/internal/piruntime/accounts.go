package piruntime

import "regexp"

// CanonicalCredentialRef names the explicitly configured native store. It is
// not a path, a default directory, or evidence of upstream account entitlement.
const CanonicalCredentialRef = "canonical"

type AccountBinding struct {
	Provider   string `json:"provider"`
	AccountRef string `json:"account_ref"`
}

var accountIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func validateAccountBindings(bindings []AccountBinding) error {
	if len(bindings) == 0 || len(bindings) > 64 {
		return ErrUnavailable
	}
	previous := ""
	for _, binding := range bindings {
		if !accountIdentifier.MatchString(binding.Provider) || !accountIdentifier.MatchString(binding.AccountRef) || (previous != "" && previous >= binding.Provider) {
			return ErrUnavailable
		}
		previous = binding.Provider
	}
	return nil
}
func bindingAllowed(bindings []AccountBinding, settings EffectiveSettings) bool {
	for _, binding := range bindings {
		if binding.Provider == settings.Provider && binding.AccountRef == settings.AccountRef {
			return true
		}
	}
	return false
}
