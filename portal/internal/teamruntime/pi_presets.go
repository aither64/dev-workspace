package teamruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ProjectPiPreset applies only to new Pi selections. CatalogDigest continues to
// identify the installed source catalog; TeamDigest binds the derived settings
// and their original team provenance. Never apply this to a retained roster.
func ProjectPiPreset(base Preset) (Preset, error) {
	if !digestPattern.MatchString(base.CatalogDigest) || !digestPattern.MatchString(base.TeamDigest) {
		return Preset{}, ErrPiPolicy
	}
	projected := base
	projected.Roles = append([]string{}, base.Roles...)
	projected.Members = append([]MemberSpec{}, base.Members...)
	model := func(value string) string {
		if strings.Contains(value, "astra") {
			return "gpt-6.2-astra"
		}
		if strings.Contains(value, "sol") {
			return "gpt-6.2-sol"
		}
		return value
	}
	projected.LeadModel = model(base.LeadModel)
	for i := range projected.Members {
		member := &projected.Members[i]
		member.Model = model(member.Model)
		if member.Role == "observer" {
			member.Model, member.Effort = "gpt-6.2-sol", "low"
		}
	}
	projected.TeamDigest = ""
	payload := struct {
		Projection       string `json:"projection"`
		SourceTeamDigest string `json:"sourceTeamDigest"`
		Preset           Preset `json:"preset"`
	}{"pi-preset-v1", base.TeamDigest, projected}
	data, err := json.Marshal(payload)
	if err != nil {
		return Preset{}, err
	}
	digest := sha256.Sum256(data)
	projected.TeamDigest = hex.EncodeToString(digest[:])
	return projected, nil
}
