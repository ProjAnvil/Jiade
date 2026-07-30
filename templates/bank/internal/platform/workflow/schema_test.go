package workflow

import (
	"os"
	"strings"
	"testing"
)

// TestPaymentMigrationContainsWorkflowSchema is a contract test that parses
// the pay_db.sql migration text and asserts the durable workflow persistence
// schema is present. The test does not execute the DDL so it can run without a
// live database; it guards against accidental removal or rename of the
// workflow_instance / workflow_action tables and their key columns.
//
// CWD for the test runner is this package directory
// (internal/platform/workflow); three levels up reaches templates/bank/.
func TestPaymentMigrationContainsWorkflowSchema(t *testing.T) {
	body, err := os.ReadFile("../../../db/migrations/pay_db.sql")
	if err != nil {
		t.Fatalf("读 pay_db.sql 失败: %v", err)
	}
	got := string(body)

	required := []string{
		"CREATE TABLE IF NOT EXISTS workflow_instance",
		"definition_version INTEGER NOT NULL",
		"prepared_context_json JSONB",
		"revision BIGINT NOT NULL",
		"lease_owner TEXT",
		"lease_until TIMESTAMPTZ",
		"CREATE TABLE IF NOT EXISTS workflow_action",
		"UNIQUE (workflow_id, action_index)",
		"idempotency_key TEXT NOT NULL",
		"command_id TEXT",
		"result_event_id TEXT",
	}
	for _, frag := range required {
		if !strings.Contains(got, frag) {
			t.Errorf("pay_db.sql 缺少必需片段: %q", frag)
		}
	}
}
