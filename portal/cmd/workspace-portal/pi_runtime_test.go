package main

import "testing"

func TestPiSupervisorCommandRefusesOtherArguments(t *testing.T) {
	valid := []string{"supervise", "--launch-request", "/fixed/launch.json", "--instance", "instance", "--request", "request", "--digest", "digest"}
	if _, err := parsePiSupervisor(valid); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{nil, {}, valid[:7], append(append([]string{}, valid...), "extra"), {"supervise", "--launch-request", "x", "--launch-request", "x", "--request", "r", "--digest", "d"}, {"supervise", "--argv", "x", "--instance", "i", "--request", "r", "--digest", "d"}} {
		if _, err := parsePiSupervisor(bad); err == nil {
			t.Fatal("invalid supervisor arguments accepted", bad)
		}
	}
}
