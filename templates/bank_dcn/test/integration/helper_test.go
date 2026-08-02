//go:build integration

// Package integration runs external integration tests from the host against the running docker stack.
// Prerequisite: make up (Skip if the stack is unreachable). Endpoints can be overridden via env vars (DCN_IT_<NAME>).
// Run: go test -tags=integration -p 1 ./test/integration/...
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	gatewayBase = envOr("DCN_IT_GATEWAY", "http://localhost:18070")
	gnsBase     = envOr("DCN_IT_GNS", "http://localhost:18080")
	rmbBase     = envOr("DCN_IT_RMB", "http://localhost:18090")
	admBase     = envOr("DCN_IT_ADM", "http://localhost:18091")
	batchBase   = envOr("DCN_IT_BATCH", "http://localhost:18092")
	consoleBase = envOr("DCN_IT_CONSOLE", "http://localhost:18099")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// unitBase maps a unit name to its host port (the endpoint returned by locate is a container-internal address, unreachable from the host).
func unitBase(dcn string) string {
	switch dcn {
	case "dcn01":
		return envOr("DCN_IT_DCN01", "http://localhost:18081")
	case "dcn02":
		return envOr("DCN_IT_DCN02", "http://localhost:18082")
	case "dcn03":
		return envOr("DCN_IT_DCN03", "http://localhost:18083")
	case "dcn04":
		return envOr("DCN_IT_DCN04", "http://localhost:18084")
	}
	return ""
}

var hc = &http.Client{Timeout: 10 * time.Second}

// probe checks the service health endpoint and Skips if unreachable (no error when the stack is not running).
func probe(t *testing.T, url string) {
	t.Helper()
	resp, err := hc.Get(url + "/healthz")
	if err != nil {
		t.Skipf("stack not running (%s): %v", url, err)
	}
	resp.Body.Close()
}

func doJSON(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// openAccount opens an account via GNS (idempotent by requestID), returning the account ID and its unit.
func openAccount(t *testing.T, gnsURL, name, requestID string) (int, string) {
	t.Helper()
	code, raw := doJSON(t, "POST", gnsURL+"/accounts", map[string]string{
		"name": name, "initBalance": "1000.00", "requestId": requestID,
	})
	if code >= 300 {
		t.Fatalf("open account: %d %s", code, raw)
	}
	var v struct {
		AccountID int    `json:"accountId"`
		DCN       string `json:"dcn"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.AccountID, v.DCN
}

// openPair opens several accounts and finds a pair in the same unit (sameUnit=true) or across units (false).
func openPair(t *testing.T, gnsURL string, sameUnit bool) (int, int, string, string) {
	t.Helper()
	type acct struct {
		id  int
		dcn string
	}
	var list []acct
	tag := fmt.Sprintf("itest-pair-%d", time.Now().UnixNano())
	for i := 0; i < 8; i++ {
		id, dcn := openAccount(t, gnsURL, "itest-account", fmt.Sprintf("%s-%d", tag, i))
		for _, a := range list {
			if (a.dcn == dcn) == sameUnit {
				return a.id, id, a.dcn, dcn
			}
		}
		list = append(list, acct{id, dcn})
	}
	t.Fatalf("no suitable account pair after 8 opens (sameUnit=%v)", sameUnit)
	return 0, 0, "", ""
}

// balance reads the account balance (returned as a string verbatim).
func balance(t *testing.T, dcn string, accountID int) string {
	t.Helper()
	code, raw := doJSON(t, "GET",
		fmt.Sprintf("%s/accounts/%d/balance", unitBase(dcn), accountID), nil)
	if code != 200 {
		t.Fatalf("balance: %d %s", code, raw)
	}
	var v struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Balance
}

// balanceSum reads the unit's total balance sum.
func balanceSum(t *testing.T, dcn string) string {
	t.Helper()
	code, raw := doJSON(t, "GET", unitBase(dcn)+"/internal/balance-sum", nil)
	if code != 200 {
		t.Fatalf("balance-sum %s: %d", dcn, code)
	}
	var v struct {
		Sum string `json:"balanceSum"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Sum
}

// decEq compares two decimal strings with float tolerance.
func decEq(a, b string) bool {
	var x, y float64
	if _, err := fmt.Sscanf(a, "%f", &x); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(b, "%f", &y); err != nil {
		return false
	}
	d := x - y
	return d*d < 0.000001
}

func decAdd(a, b string) string {
	var x, y float64
	fmt.Sscanf(a, "%f", &x)
	fmt.Sscanf(b, "%f", &y)
	return fmt.Sprintf("%.2f", x+y)
}

// contains asserts the body contains the substring.
func contains(t *testing.T, raw []byte, substr string) {
	t.Helper()
	if !strings.Contains(string(raw), substr) {
		t.Fatalf("response missing %q: %s", substr, raw)
	}
}
