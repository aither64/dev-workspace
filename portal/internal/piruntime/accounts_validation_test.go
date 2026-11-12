package piruntime

import "testing"

func TestAccountBindingValidation(t *testing.T) {
	for _, bindings := range [][]AccountBinding{nil, {}, {{Provider: "p", AccountRef: "a"}, {Provider: "p", AccountRef: "b"}}, {{Provider: "z", AccountRef: "a"}, {Provider: "a", AccountRef: "b"}}, {{Provider: "P", AccountRef: "a"}}, {{Provider: "p", AccountRef: "../account"}}} {
		if validateAccountBindings(bindings) == nil {
			t.Fatal("invalid account map accepted", bindings)
		}
	}
	if err := validateAccountBindings([]AccountBinding{{Provider: "a", AccountRef: "one"}, {Provider: "b", AccountRef: "two"}}); err != nil {
		t.Fatal(err)
	}
}
