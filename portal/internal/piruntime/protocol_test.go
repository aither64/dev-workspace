package piruntime

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestFrozenLaunchBytes(t *testing.T) {
	data, err := os.ReadFile("../../../test/fixtures/pi-host-launch-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("../../../test/fixtures/pi-host-launch-v1.json.sha256")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := DecodeLaunch(data)
	if err != nil {
		t.Fatal(err)
	}
	if digest != strings.TrimSpace(string(want)) {
		t.Fatalf("byte digest %s", digest)
	}
	_, reformatted, err := DecodeLaunch(bytes.TrimSpace(data))
	if err != nil {
		t.Fatal(err)
	}
	if reformatted == digest {
		t.Fatal("newline was not bound")
	}
	for _, bad := range [][]byte{bytes.Replace(data, []byte(`"schema": 1`), []byte(`"schema": 1, "schema": 1`), 1), bytes.Replace(data, []byte(`"schema": 1`), []byte(`"schema": null`), 1), append(data, []byte(`{}`)...), bytes.Replace(data, []byte(`"schema": 1`), []byte(`"schema": 2`), 1)} {
		if _, _, err := DecodeLaunch(bad); err == nil {
			t.Fatal("malformed/versioned launch accepted")
		}
	}
}
