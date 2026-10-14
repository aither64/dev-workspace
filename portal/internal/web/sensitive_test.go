package web

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSensitivePreparationIsPartOfImmutableRequest(t *testing.T) {
	values := url.Values{"clientRequestId": {"12345678-1234-4123-8123-123456789abc"}, "goal": {"local request"}, "creation_date": {"2026-10-14"}, "sensitive": {"true"}}
	request := httptest.NewRequest("POST", "/sessions", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	_, input, err := preparationForm(request)
	if err != nil || !input.Sensitive {
		t.Fatalf("input=%#v err=%v", input, err)
	}
	privateDigest := inputPreparationDigest(input)
	creation := preparationCreationRequest(sessionPreparation{Slug: "2026-10-14-private", Snapshot: &preparationSnapshot{Input: input, Goal: input.RawPrompt}})
	if !creation.Sensitive {
		t.Fatal("sensitivity lost at handoff")
	}
	input.Sensitive = false
	if privateDigest == inputPreparationDigest(input) {
		t.Fatal("sensitivity missing from immutable identity")
	}
	request.Form.Set("sensitive", "false")
	if _, _, err := preparationForm(request); err == nil {
		t.Fatal("accepted invalid sensitivity")
	}
}

func TestSensitiveReceiptMarkerIsStrict(t *testing.T) {
	input := creationRequest{Kind: "new", Slug: "sensitive", Goal: "local", Sensitive: true}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["sensitive"]) != "true" {
		t.Fatal("marker missing")
	}
	input.Sensitive = false
	encoded, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var ordinary map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &ordinary); err != nil {
		t.Fatal(err)
	}
	if _, present := ordinary["sensitive"]; present {
		t.Fatal("ordinary request encoding changed")
	}
}
