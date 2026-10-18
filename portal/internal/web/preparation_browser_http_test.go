package web

import (
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"testing"
)

// Run in the ordinary package suite: no Playwright flag or mocked POST route.
func TestSessionPreparationShippedBrowserHTTP(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is supplied by the Nix check environment")
	}
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%t", managed), func(t *testing.T) {
			server := creationSettingsTestServer(t)
			if !managed {
				server.installedTeams = nil
			}
			httpServer := httptest.NewUnstartedServer(nil)
			server.config.BaseURL = "http://" + httpServer.Listener.Addr().String()
			server.trustedOrigins = []string{server.config.BaseURL}
			httpServer.Config.Handler = server.Handler()
			httpServer.Start()
			defer httpServer.Close()
			command := exec.Command(node, "preparation_browser_http_test.cjs", httpServer.URL, strconv.FormatBool(managed))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("shipped preparation HTTP contract: %v\n%s", err, output)
			} else {
				t.Log(string(output))
			}
			server.operationMu.Lock()
			defer server.operationMu.Unlock()
			for index, provider := range []string{"openai", "pool", ""} {
				id := preparationTestID(index + 1)
				record, found := server.preparations[id]
				if !found || record.Snapshot == nil || record.Snapshot.Input.Provider != provider {
					t.Fatalf("request %s: provider not durably accepted: %#v", id, record)
				}
			}
		})
	}
}
