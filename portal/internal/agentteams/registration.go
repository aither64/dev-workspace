package agentteams

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const RegistrationPolicyVersion = 1

// RegistrationPlan is the host launch contract. Direct team members use
// independent App Server threads, so they need no native child-agent configuration.
// The installed catalog identity still participates in the marker digest: a
// creation request uses the selected package's validated catalog. The digest
// records launch provenance; catalog changes alone do not alter the native
// executable or capacity. WithWorkspaceReporting records the exact owned startup
// server argv. The host validates policy-1 reporting invocations before allowing
// a package-only command-path change without restarting the native daemon.
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

// WithWorkspaceReporting adds only this workspace's owned MCP server. Individual
// dotted overrides preserve unrelated native MCP servers and their settings.
func (plan RegistrationPlan) WithWorkspaceReporting(packageRoot, stateRoot, workspace, hostProfile string) (RegistrationPlan, error) {
	for _, path := range []string{packageRoot, stateRoot, workspace, hostProfile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
			return RegistrationPlan{}, errors.New("reporting registration requires canonical absolute authorities")
		}
	}
	values := []struct {
		key   string
		value any
	}{
		{"command", filepath.Join(packageRoot, "bin", "workspace-portal")},
		{"args", []string{"team-mcp", "--user-state-root", stateRoot, "--workspace", workspace, "--host-profile", hostProfile}},
		{"required", true},
		{"enabled_tools", []string{"report_to_lead"}},
		{"tools.report_to_lead.approval_mode", "approve"},
	}
	for _, value := range values {
		encoded, err := json.Marshal(value.value)
		if err != nil {
			return RegistrationPlan{}, err
		}
		plan.Argv = append(plan.Argv, "-c", "mcp_servers.team_reporting."+value.key+"="+string(encoded))
	}
	digest, err := digestJSON(struct {
		Base            string   `json:"base"`
		ReportingPolicy int      `json:"reporting_policy"`
		Argv            []string `json:"argv"`
	}{plan.Digest, 1, plan.Argv})
	if err != nil {
		return RegistrationPlan{}, err
	}
	plan.Digest = digest
	return plan, nil
}
