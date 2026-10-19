package web

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

func TestIndexRefreshPreservesSelection(t *testing.T) {
	if os.Getenv("PORTAL_BROWSER_TEST") != "1" {
		t.Skip("requires browser environment")
	}
	server := newTestServer(t)
	httpServer := httptest.NewTLSServer(server.Handler())
	defer httpServer.Close()
	command := exec.Command("node", "index_refresh_browser_test.cjs", httpServer.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	}
}
