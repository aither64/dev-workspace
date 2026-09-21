package agentteams

import (
	"errors"
	"fmt"
)

const RegistrationPolicyVersion = 1

// RegistrationPlan is the host launch contract. Direct team members use
// independent App Server threads, so they need no native agent configuration.
// The installed catalog identity still participates in the marker digest: a
// creation request can only use a catalog registered with this host generation.
type RegistrationPlan struct {
	Schema                     int      `json:"schema"`
	Policy                     int      `json:"policy"`
	Argv                       []string `json:"argv"`
	Digest                     string   `json:"digest"`
	RequiredNativeChildThreads int      `json:"required_native_child_threads"`
	States                     []any    `json:"states"`
}

type registrationSemanticPlan struct {
	Policy        int    `json:"policy"`
	CatalogDigest string `json:"catalog_digest"`
}

// BuildRegistrationPlan validates the exact installed package and returns
// direct-thread launch evidence. Retired virtual state does not contribute to
// the plan or prevent an idle session from restarting after the cutover.
func BuildRegistrationPlan(packageRoot string) (RegistrationPlan, error) {
	return buildRegistrationPlan(packageRoot, LoadInstalled)
}

func buildRegistrationPlan(packageRoot string, load func(string) (Installed, error)) (RegistrationPlan, error) {
	if !nixStoreRoot(packageRoot) {
		return RegistrationPlan{}, errors.New("agent team registration requires a canonical package store root")
	}
	installed, err := load(packageRoot)
	if err != nil {
		return RegistrationPlan{}, fmt.Errorf("load current package for agent registration: %w", err)
	}
	if installed.packageRoot != packageRoot {
		return RegistrationPlan{}, errors.New("current agent team package was not loaded from its registration root")
	}
	var catalogDigest string
	if installed.Managed {
		if installed.Catalog == nil || installed.Catalog.Validate() != nil ||
			installed.Metadata.AgentTeams.Catalog.Digest != installed.Catalog.CatalogDigest {
			return RegistrationPlan{}, errors.New("installed agent team catalog is invalid")
		}
		catalogDigest = installed.Catalog.CatalogDigest
	} else if installed.Catalog != nil {
		return RegistrationPlan{}, errors.New("unmanaged package has an agent team catalog")
	}
	digest, err := digestJSON(registrationSemanticPlan{Policy: RegistrationPolicyVersion, CatalogDigest: catalogDigest})
	if err != nil {
		return RegistrationPlan{}, fmt.Errorf("digest agent team registration plan: %w", err)
	}
	return RegistrationPlan{
		Schema: 1, Policy: RegistrationPolicyVersion, Argv: []string{}, Digest: digest,
		RequiredNativeChildThreads: 0, States: []any{},
	}, nil
}
