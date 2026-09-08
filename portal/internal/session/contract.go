package session

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
)

//go:embed runtime-contract.json
var runtimeContractJSON []byte

type LifecycleJournal struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

type runtimeContract struct {
	TrackingMaxBytes  int                `json:"trackingMaxBytes"`
	LifecycleJournals []LifecycleJournal `json:"lifecycleJournals"`
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
	return contract
}

// LifecycleJournals returns the authoritative persisted journal names and
// their public recovery commands.
func LifecycleJournals() []LifecycleJournal {
	return append([]LifecycleJournal(nil), sharedRuntimeContract.LifecycleJournals...)
}

// These values project the shared request boundary and worst-case transport
// encoding overhead published in runtime-contract.json.
const (
	MaxMessageBytes         = 20_000
	FormEncodingExpansion   = 3
	JSONEncodingExpansion   = 6
	TransportEnvelopeBytes  = 1_024
	MaxFormRequestBodyBytes = MaxMessageBytes*FormEncodingExpansion + TransportEnvelopeBytes
	MaxJSONRequestBodyBytes = MaxMessageBytes*JSONEncodingExpansion + TransportEnvelopeBytes
)

// FormattedMaxMessageBytes returns the published limit for user-visible text.
func FormattedMaxMessageBytes() string {
	digits := strconv.Itoa(MaxMessageBytes)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}
