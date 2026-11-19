package teamruntime

import (
	"reflect"
	"strings"
	"testing"
)

func TestPiPresetProjectionOnlyChangesNewModelChoices(t *testing.T) {
	base := Preset{ID: "delegated", CatalogDigest: strings.Repeat("a", 64), TeamDigest: strings.Repeat("b", 64), LeadModel: "gpt-6.1-sol", LeadEffort: "xhigh", LeadInstructions: "Coordinate.", Roles: []string{"lead", "architect0", "observer0"}, Members: []MemberSpec{{Role: "architect", Address: "architect0", Model: "gpt-6-astra", Effort: "xhigh", Instructions: "Design.", Access: "workspace_write"}, {Role: "observer", Address: "observer0", Model: "gpt-6.1-sol", Effort: "xhigh", Instructions: "Observe.", Access: "read_only"}}}
	before := base
	before.Roles = append([]string{}, base.Roles...)
	before.Members = append([]MemberSpec{}, base.Members...)
	projected, err := ProjectPiPreset(base)
	if err != nil || projected.LeadModel != "gpt-6.2-sol" || projected.Members[0].Model != "gpt-6.2-astra" || projected.Members[1].Model != "gpt-6.2-sol" || projected.Members[1].Effort != "xhigh" || projected.CatalogDigest != base.CatalogDigest || projected.TeamDigest == base.TeamDigest {
		t.Fatal(projected, err)
	}
	expected := before
	expected.Members = append([]MemberSpec{}, before.Members...)
	expected.LeadModel, expected.TeamDigest = "gpt-6.2-sol", projected.TeamDigest
	expected.Members[0].Model, expected.Members[1].Model = "gpt-6.2-astra", "gpt-6.2-sol"
	if !reflect.DeepEqual(projected, expected) {
		t.Fatal("projection changed role settings beyond model choices", projected)
	}
	again, err := ProjectPiPreset(base)
	if err != nil || !reflect.DeepEqual(again, projected) || !reflect.DeepEqual(base, before) {
		t.Fatal("projection mutated source or changed digest")
	}
	projected.Members[0].Instructions = "Changed"
	if base.Members[0].Instructions != "Design." {
		t.Fatal("member slice aliases source")
	}
	changed := base
	changed.TeamDigest = strings.Repeat("c", 64)
	other, _ := ProjectPiPreset(changed)
	if other.TeamDigest == again.TeamDigest {
		t.Fatal("projection lost original team provenance")
	}
	configured := before
	configured.Members = append([]MemberSpec{}, before.Members...)
	configured.Members[1].Effort = "medium"
	custom, err := ProjectPiPreset(configured)
	if err != nil || custom.Members[1].Effort != "medium" || custom.TeamDigest == again.TeamDigest || configured.Members[1].Effort != "medium" || before.Members[1].Effort != "xhigh" {
		t.Fatal("projection lost configured observer effort or source identity", custom, err)
	}
}
