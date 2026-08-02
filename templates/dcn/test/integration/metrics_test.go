//go:build integration

package integration

import (
	"strings"
	"testing"
)

// 各服务 /metrics 暴露本服务 RED 指标（CounterVec 首次计数才产出序列，故先发一条已埋点请求）。
func TestMetricsEndpoints(t *testing.T) {
	services := []struct {
		name string
		base string
		hit  string // 已埋点的请求路径（/healthz 未埋点，不可用）
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
		doJSON(t, "GET", s.base+s.hit, nil) // 触发计数（404 也计入）
		_, raw := doJSON(t, "GET", s.base+"/metrics", nil)
		// exposition 标签按字母序排列（code,handler,service），不能按 {service= 前缀匹配。
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
