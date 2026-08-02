//go:build integration

package integration

import (
	"strings"
	"testing"
)

// Console proxy endpoints: targets (Prometheus) and containers (Docker API) are reachable and return valid JSON.
func TestConsoleProxies(t *testing.T) {
	probe(t, consoleBase)
	code, raw := doJSON(t, "GET", consoleBase+"/api/targets", nil)
	if code != 200 || !strings.Contains(string(raw), `"status":"success"`) {
		t.Fatalf("targets: %d %s", code, raw)
	}
	code, raw = doJSON(t, "GET", consoleBase+"/api/containers", nil)
	if code != 200 || !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Fatalf("containers: %d %.200s", code, raw)
	}
}
