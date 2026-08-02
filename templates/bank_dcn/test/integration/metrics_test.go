//go:build integration

package integration

import (
	"strings"
	"testing"
)

// Each service's /metrics exposes its own RED metrics (a CounterVec only emits a series after its first count, so send one instrumented request first).
func TestMetricsEndpoints(t *testing.T) {
	services := []struct {
		name string
		base string
		hit  string // instrumented request path (/healthz is not instrumented, unusable)
	}{
		{"gns", gnsBase, "/locate?accountId=1"},
		{"rmb-coordinator", rmbBase, "/transactions/nonexistent"},
		{"adm", admBase, "/report/summary"},
		{"batch-scheduler", batchBase, "/jobs/1900-01-01"},
		{"dcn01", unitBase("dcn01"), "/accounts/1/balance"},
		{"dcn02", unitBase("dcn02"), "/accounts/1/balance"},
		{"dcn03", unitBase("dcn03"), "/accounts/1/balance"},
		{"console", consoleBase, "/api/targets"},
	}
	for _, s := range services {
		probe(t, s.base)
		doJSON(t, "GET", s.base+s.hit, nil) // trigger a count (404 counts too)
		_, raw := doJSON(t, "GET", s.base+"/metrics", nil)
		// Exposition labels are sorted alphabetically (code,handler,service), so a {service= prefix match is not possible.
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "http_requests_total{") && strings.Contains(line, `service="`+s.name+`"`) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s /metrics missing its series", s.name)
		}
	}
}
