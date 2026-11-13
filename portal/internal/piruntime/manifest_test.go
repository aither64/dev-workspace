package piruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWorkerManifest(t *testing.T) {
	valid := `{"schema":1,"worker_protocol":1,"node_executable":"/nix/store/00000000000000000000000000000000-node/bin/node","worker_entrypoint":"/nix/store/00000000000000000000000000000000-worker/worker.mjs","pi_package_root":"/nix/store/00000000000000000000000000000000-pi"}`
	if _, err := DecodeWorkerMetadata([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(valid, `"schema":1`, `"Schema":1`, 1), strings.Replace(valid, `"schema":1`, `"schema":1,"Schema":1`, 1), strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(valid, `"schema":1`, `"extra":1,"schema":1`, 1), strings.Replace(valid, `"schema":1`, `"schema":null`, 1), strings.Replace(valid, `"worker_protocol":1`, `"worker_protocol":2`, 1), valid + `{}`, strings.Replace(valid, "/nix/store/00000000000000000000000000000000-node/bin/node", "/tmp/node", 1), strings.Replace(valid, `"node_executable":"/nix/store/00000000000000000000000000000000-node/bin/node"`, `"node_executable":null`, 1)} {
		if _, err := DecodeWorkerMetadata([]byte(bad)); err == nil {
			t.Fatalf("invalid manifest accepted: %s", bad)
		}
	}
	pi := os.Getenv("PI_PROBE_PACKAGE")
	if pi == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(pi, "bin/pi"))
	if err != nil {
		t.Fatal(err)
	}
	node := regexp.MustCompile(`/nix/store/[a-z0-9]{32}-nodejs-[^/\s"']+/bin/node`).FindString(string(data))
	metadata := workerMetadata{Schema: 1, Protocol: 1, Node: node, Entrypoint: node, PiPackage: pi}
	encoded, _ := json.Marshal(metadata)
	if _, err := DecodeWorkerMetadata(encoded); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkerPaths(metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Node = filepath.Join(pi, "bin/missing")
	if err := validateWorkerPaths(metadata); err == nil {
		t.Fatal("missing executable accepted")
	}
	metadata.Node = filepath.Join(pi, "lib/pi")
	if err := validateWorkerPaths(metadata); err == nil {
		t.Fatal("directory executable accepted")
	}
	metadata.Node = node
	metadata.Entrypoint = filepath.Join(pi, "lib/pi")
	if err := validateWorkerPaths(metadata); err == nil {
		t.Fatal("directory entrypoint accepted")
	}
}
