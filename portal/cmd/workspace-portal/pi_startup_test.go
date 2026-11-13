package main

import "testing"

func TestPiAuthStartupFlagPreservesExplicitHostConfiguration(t *testing.T) {
	flags, options := newServeFlagSet()
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if options.piAuthDirectory != "" || options.piHostStateRoot != "" {
		t.Fatal("missing Pi configuration acquired a default")
	}
	flags, options = newServeFlagSet()
	value := "/srv/shared Pi credentials"
	if err := flags.Parse([]string{"--pi-auth-dir", value, "--pi-host-state-root", "/srv/explicit Pi host state", "--user-state-root", "/srv/private-state"}); err != nil {
		t.Fatal(err)
	}
	if options.piAuthDirectory != value || options.piHostStateRoot != "/srv/explicit Pi host state" || options.userStateRoot != "/srv/private-state" {
		t.Fatalf("trusted startup paths were changed: %#v", options)
	}
}
