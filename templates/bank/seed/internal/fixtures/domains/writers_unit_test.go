package domains

// Unit tests for the fixture writers and bulk-insert helpers using the
// fakesql harness in fakesql_test.go (no live PostgreSQL). The generators
// (Gen*) stay covered by their deterministic tests; these tests cover the SQL
// writing paths: NULL handling, batching, arg marshalling, and error wrapping.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bank/seed/internal/fixtures"
)

// writerDB returns a fake DB accepting every DELETE and INSERT, plus the db
// handle itself for assertions.
func writerDB() (*fakeDB, struct{}) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "delete from", affected: 0})
	db.add(queryPlan{marker: "insert into", affected: 1})
	return db, struct{}{}
}

func TestWriteStatic_ExecutesEveryBatch(t *testing.T) {
	db, _ := writerDB()
	data := GenStaticData(fixtures.DefaultConfig(fixtures.ScaleDev))
	if err := WriteStatic(context.Background(), db.open(), data); err != nil {
		t.Fatalf("WriteStatic: %v", err)
	}
	// 5 table clears + one insert per static row must all hit the DB.
	if got := len(db.callsFor("delete from")); got != 5 {
		t.Errorf("table clears = %d, want 5", got)
	}
	if got := len(db.callsFor("insert into sys_param")); got != len(data.SysParams) {
		t.Errorf("sys_param inserts = %d, want %d", got, len(data.SysParams))
	}
	if got := len(db.callsFor("insert into branch")); got != len(data.Branches) {
		t.Errorf("branch inserts = %d, want %d", got, len(data.Branches))
	}
}

func TestWriteStatic_ClearFailureSurfaces(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "delete from", err: errors.New("permission denied")})
	if err := WriteStatic(context.Background(), db.open(), StaticData{}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("want wrapped delete error, got %v", err)
	}
}

func TestWriteAccounts_NullableParentsAndAmounts(t *testing.T) {
	db, _ := writerDB()
	demand, fixed := GenAccountRows(fixtures.DefaultConfig(fixtures.ScaleDev))
	if len(demand) == 0 || len(fixed) == 0 {
		t.Fatal("generator must produce demand and fixed accounts")
	}
	if err := WriteAccounts(context.Background(), db.open(), demand, fixed); err != nil {
		t.Fatalf("WriteAccounts: %v", err)
	}
	if got := len(db.callsFor("insert into demand_account")); got != len(demand) {
		t.Errorf("demand inserts = %d, want %d", got, len(demand))
	}
	if got := len(db.callsFor("insert into fixed_account")); got != len(fixed) {
		t.Errorf("fixed inserts = %d, want %d", got, len(fixed))
	}
	// The first fixed insert must serialise principal as a NUMERIC string.
	call := db.lastCallWith("insert into fixed_account")
	if call == nil || !strings.Contains(call.args[3].(string), ".") {
		t.Errorf("principal must be a decimal string, got %#v", call.args[3])
	}
}

func TestWriteCustomers_NullFieldsAndRiskTags(t *testing.T) {
	db, _ := writerDB()
	rows := GenCustomers(fixtures.DefaultConfig(fixtures.ScaleDev), 3)
	if len(rows) != 3 {
		t.Fatalf("customers = %d, want 3", len(rows))
	}
	if err := WriteCustomers(context.Background(), db.open(), rows); err != nil {
		t.Fatalf("WriteCustomers: %v", err)
	}
	if got := len(db.callsFor("insert into cust_info")); got != 3 {
		t.Errorf("cust_info inserts = %d, want 3", got)
	}
	// risk_tags must be encoded as a JSON array string for the ARRAY cast.
	call := db.lastCallWith("insert into cust_info")
	if call == nil || !strings.HasPrefix(call.args[11].(string), "[") {
		t.Errorf("risk tags must be a JSON array, got %#v", call.args[11])
	}
}

func TestWriteAccountRels(t *testing.T) {
	db, _ := writerDB()
	rels := GenAccountRels([][2]string{{"C1", "A1"}, {"C2", "A2"}})
	if err := WriteAccountRels(context.Background(), db.open(), rels); err != nil {
		t.Fatalf("WriteAccountRels: %v", err)
	}
	if got := len(db.callsFor("insert into cust_account_rel")); got != 2 {
		t.Errorf("rel inserts = %d, want 2", got)
	}
}
