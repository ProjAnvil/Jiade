//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// End-of-day batch (yesterday's bizDate, avoiding verify gate 8's same-day task):
// SUCCEEDED → retriggering is idempotent (totalInterest unchanged, balance sums not credited twice).
func TestInterestBatchIdempotent(t *testing.T) {
	probe(t, batchBase)
	openAccount(t, gnsBase, "itest-account", "itest-batch-seed") // ensure at least one account exists
	bizDate := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	code, raw := doJSON(t, "POST", gatewayBase+"/batch/jobs/interest",
		map[string]string{"bizDate": bizDate})
	if code != 200 {
		t.Fatalf("batch trigger: %d %s", code, raw)
	}
	var job struct {
		Status        string `json:"status"`
		TotalInterest string `json:"totalInterest"`
	}
	if err := json.Unmarshal(raw, &job); err != nil || job.Status != "SUCCEEDED" {
		t.Fatalf("job = %s", raw)
	}
	sum1 := decAdd(decAdd(balanceSum(t, "dcn01"), balanceSum(t, "dcn02")), balanceSum(t, "dcn03"))

	code, raw = doJSON(t, "POST", gatewayBase+"/batch/jobs/interest",
		map[string]string{"bizDate": bizDate})
	if code != 200 {
		t.Fatalf("retrigger: %d", code)
	}
	var job2 struct {
		TotalInterest string `json:"totalInterest"`
	}
	json.Unmarshal(raw, &job2)
	if !decEq(job2.TotalInterest, job.TotalInterest) {
		t.Fatal("retrigger totalInterest drifted")
	}
	sum2 := decAdd(decAdd(balanceSum(t, "dcn01"), balanceSum(t, "dcn02")), balanceSum(t, "dcn03"))
	if !decEq(sum1, sum2) {
		t.Fatal("retrigger credited interest twice")
	}
}
