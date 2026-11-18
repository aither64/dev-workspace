package teamruntime

import (
	"reflect"
	"strings"
	"testing"
)

func TestPiPresetProjectionOnlyChangesNewModelChoices(t *testing.T) {
	base := Preset{ID: "delegated", CatalogDigest: strings.Repeat("a", 64), TeamDigest: strings.Repeat("b", 64), LeadModel: "gpt-6.1-sol", LeadEffort: "xhigh", LeadInstructions: "Coordinate.", Roles: []string{"lead", "architect0", "observer0"}, Members: []MemberSpec{{Role: "architect", Address: "architect0", Model: "gpt-6-astra", Effort: "xhigh", Instructions: "Design.", Access: "workspace_write"}, {Role: "observer", Address: "observer0", Model: "gpt-6.1-sol", Effort: "xhigh", Instructions: "Observe.", Access: "read_only"}}}
	before := base
	projected, err := ProjectPiPreset(base)
	if err != nil || projected.LeadModel != "gpt-6.2-sol" || projected.Members[0].Model != "gpt-6.2-astra" || projected.Members[1].Model != "gpt-6.2-sol" || projected.Members[1].Effort != "low" || projected.CatalogDigest != base.CatalogDigest || projected.TeamDigest == base.TeamDigest {
		t.Fatal(projected, err)
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
}
