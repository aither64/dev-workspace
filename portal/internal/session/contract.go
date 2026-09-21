package session

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

//go:embed runtime-contract.json
var runtimeContractJSON []byte

type LifecycleJournal struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type TmuxMetadataValue struct {
	Name string `json:"name"`
	Path bool   `json:"path"`
}

type TmuxMetadata struct {
	AuthorityFormat []string            `json:"authorityFormat"`
	SessionOptions  []TmuxMetadataValue `json:"sessionOptions"`
	WindowOptions   []TmuxMetadataValue `json:"windowOptions"`
	Environment     []TmuxMetadataValue `json:"environment"`
}

type ClusterProviderContract struct {
	StatusBusyExitCode    int `json:"statusBusyExitCode"`
	ReleaseTimeoutSeconds int `json:"releaseTimeoutSeconds"`
}

func ClusterProvider() ClusterProviderContract { return sharedRuntimeContract.ClusterProvider }

// AgentTeamRegistrationContract is the host/helper boundary for pinned-team
// registration. The host owns marker and retry records while
// the package helper remains the authority for catalog and retained-state
// validation.
type AgentTeamRegistrationContract struct {
	HelperSchema            int    `json:"helperSchema"`
	Policy                  int    `json:"policy"`
	MarkerSchema            int    `json:"markerSchema"`
	PendingSchema           int    `json:"pendingSchema"`
	MaxPersistedScalarBytes int    `json:"maxPersistedScalarBytes"`
	MaxOutputBytes          int    `json:"maxOutputBytes"`
	MigrationJournalSuffix  string `json:"migrationJournalSuffix"`
}

// AgentTeamRegistration returns the immutable host/helper contract.
func AgentTeamRegistration() AgentTeamRegistrationContract {
	return sharedRuntimeContract.AgentTeamRegistration
}

type runtimeContract struct {
	ClusterProvider       ClusterProviderContract       `json:"clusterProvider"`
	AgentTeamRegistration AgentTeamRegistrationContract `json:"agentTeamRegistration"`
	PortalServeFlags      []string                      `json:"portalServeFlags"`
	TrackingMaxBytes      int                           `json:"trackingMaxBytes"`
	LifecycleJournals     []LifecycleJournal            `json:"lifecycleJournals"`
	TmuxMetadata          TmuxMetadata                  `json:"tmuxMetadata"`
}

var sharedRuntimeContract = mustLoadRuntimeContract()

func mustLoadRuntimeContract() runtimeContract {
	var contract runtimeContract
	if err := json.Unmarshal(runtimeContractJSON, &contract); err != nil {
		panic(fmt.Sprintf("decode embedded workspace runtime contract: %v", err))
	}
	if contract.TrackingMaxBytes != TrackingMaxSize {
		panic("workspace runtime contract has a mismatched tracking limit")
	}
	if contract.ClusterProvider.StatusBusyExitCode != 75 || contract.ClusterProvider.ReleaseTimeoutSeconds <= 0 {
		panic("invalid cluster provider contract")
	}
	registration := contract.AgentTeamRegistration
	if registration.HelperSchema != 1 || registration.Policy != 1 || registration.MarkerSchema != 1 ||
		registration.PendingSchema != 1 || registration.MaxPersistedScalarBytes != MaxAgentTeamPersistedScalarBytes || registration.MaxOutputBytes != 4*1024*1024 ||
		registration.MigrationJournalSuffix != ".agent-teams-migration.json" {
		panic("invalid agent team registration contract")
	}
	portalFlags := make(map[string]bool, len(contract.PortalServeFlags))
	for _, flag := range contract.PortalServeFlags {
		if flag == "" || portalFlags[flag] {
			panic("invalid portal serve flag contract")
		}
		portalFlags[flag] = true
	}
	for _, required := range []string{"--package-root", "--workspace-name", "--registration-marker"} {
		if !portalFlags[required] {
			panic("portal serve contract lacks agent team authority")
		}
	}
	seenNames := make(map[string]struct{})
	seenCommands := make(map[string]struct{})
	for _, journal := range contract.LifecycleJournals {
		if !slugPattern.MatchString(journal.Name) ||
			(journal.Command != "archive" && journal.Command != "delete" && journal.Command != "revive") {
			panic("workspace runtime contract has an invalid lifecycle journal")
		}
		if _, duplicate := seenNames[journal.Name]; duplicate {
			panic("workspace runtime contract repeats a lifecycle journal")
		}
		if _, duplicate := seenCommands[journal.Command]; duplicate {
			panic("workspace runtime contract repeats a lifecycle command")
		}
		seenNames[journal.Name] = struct{}{}
		seenCommands[journal.Command] = struct{}{}
	}
	if len(seenCommands) != 3 {
		panic("workspace runtime contract must map every lifecycle command")
	}
	validateTmuxMetadata(contract.TmuxMetadata)
	return contract
}

func validateTmuxMetadata(metadata TmuxMetadata) {
	expectedAuthorityFormat := []string{
		"#{session_id}",
		"#{session_name}",
		"#{@dev_session}",
		"#{@dev_session_slug}",
		"#{E:DEV_SESSION_WORKSPACE}",
		"#{E:DEV_SESSION_SLUG}",
		"#{socket_path}",
		"#{@dev_session_codex_thread}",
		"#{@dev_session_codex_socket}",
		"#{@dev_session_codex_version}",
		"#{@dev_session_codex_pane}",
		"#{E:DEV_SESSION_TMUX_IDENTITY}",
	}
	if !slices.Equal(metadata.AuthorityFormat, expectedAuthorityFormat) {
		panic("workspace runtime contract has an invalid tmux authority format")
	}
	validateTmuxMetadataValues(metadata.SessionOptions, "@dev_session", 6)
	validateTmuxMetadataValues(metadata.WindowOptions, "@dev_session_", 2)
	validateTmuxMetadataValues(metadata.Environment, "DEV_SESSION_", 17)
}

func validateTmuxMetadataValues(values []TmuxMetadataValue, prefix string, expected int) {
	if len(values) != expected {
		panic("workspace runtime contract has an invalid tmux metadata inventory")
	}
	seen := make(map[string]struct{})
	for _, value := range values {
		if !strings.HasPrefix(value.Name, prefix) || strings.ContainsAny(value.Name, "\t\r\n=") {
			panic("workspace runtime contract has an invalid tmux metadata name")
		}
		if _, duplicate := seen[value.Name]; duplicate {
			panic("workspace runtime contract repeats a tmux metadata name")
		}
		seen[value.Name] = struct{}{}
	}
}

// LifecycleJournals returns the authoritative persisted journal names and
// their public recovery commands.
func LifecycleJournals() []LifecycleJournal {
	return append([]LifecycleJournal(nil), sharedRuntimeContract.LifecycleJournals...)
}

func tmuxAuthorityFormat() string {
	return strings.Join(sharedRuntimeContract.TmuxMetadata.AuthorityFormat, "\t")
}

// These values project the shared request boundary and worst-case transport
// encoding overhead published in runtime-contract.json.
const (
	MaxMessageBytes = 20_000
	// MaxAgentTeamPersistedScalarBytes bounds model and reasoning-effort values
	// that cross the agent-team receipt, journal, catalog, and state boundary.
	MaxAgentTeamPersistedScalarBytes = 4_096
	FormEncodingExpansion            = 3
	JSONEncodingExpansion            = 6
	TransportEnvelopeBytes           = 1_024
	MaxFormRequestBodyBytes          = MaxMessageBytes*FormEncodingExpansion + TransportEnvelopeBytes
	MaxJSONRequestBodyBytes          = MaxMessageBytes*JSONEncodingExpansion + TransportEnvelopeBytes
)

// FormattedMaxMessageBytes returns the published limit for user-visible text.
func FormattedMaxMessageBytes() string {
	digits := strconv.Itoa(MaxMessageBytes)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}
