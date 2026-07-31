package migrate

import (
	"os"
	"strings"
	"testing"
)

func TestSplitStatements_DropsEmptyAndTrims(t *testing.T) {
	ddl := "  CREATE TABLE a(x int);\n\n;  CREATE TABLE b(y int);  "
	got := SplitStatements(ddl)
	want := 2
	if len(got) != want {
		t.Fatalf("want %d statements, got %d: %#v", want, len(got), got)
	}
	if strings.Contains(got[0], ";") {
		t.Errorf("statement should not contain trailing semicolon: %q", got[0])
	}
}

func TestSplitStatements_Empty(t *testing.T) {
	if got := SplitStatements("  ;  \n; "); len(got) != 0 {
		t.Errorf("want 0 statements, got %d", len(got))
	}
}

// TestSplitStatements_DollarQuotedBodyKeepsInternalSemicolons locks in the
// PL/pgSQL handling: a function/trigger body wrapped in $$ ... $$ (or a tagged
// $body$ ... $body$) may contain semicolons that must NOT split the statement.
func TestSplitStatements_DollarQuotedBodyKeepsInternalSemicolons(t *testing.T) {
	ddl := `CREATE TABLE a(x int);
CREATE OR REPLACE FUNCTION immut() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'immutable: %', TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER immut_no_update BEFORE UPDATE ON a FOR EACH ROW EXECUTE FUNCTION immut();`
	stmts := SplitStatements(ddl)
	if len(stmts) != 3 {
		t.Fatalf("want 3 statements, got %d: %#v", len(stmts), stmts)
	}
	for _, s := range stmts {
		if !isSupportedSchemaStatement(s) {
			t.Errorf("non-DDL statement after split: %q", s)
		}
	}
	// The CREATE FUNCTION statement must include its full body, including the
	// inner "END;" and the closing "$$ LANGUAGE plpgsql".
	if !strings.Contains(stmts[1], "END;") || !strings.Contains(stmts[1], "$$ LANGUAGE plpgsql") {
		t.Errorf("CREATE FUNCTION split lost body content: %q", stmts[1])
	}
	// A tagged dollar quote ($body$) must round-trip too.
	tagged := `CREATE FUNCTION t() RETURNS void AS $body$
BEGIN
    PERFORM 1;
END;
$body$ LANGUAGE plpgsql;`
	got := SplitStatements(tagged)
	if len(got) != 1 {
		t.Fatalf("tagged dollar quote: want 1 statement, got %d: %#v", len(got), got)
	}
}

func TestSupportedSchemaStatement(t *testing.T) {
	tests := []struct {
		statement string
		want      bool
	}{
		{"CREATE TABLE customer(id text)", true},
		{"ALTER TABLE customer ADD COLUMN status text", true},
		{"DROP TRIGGER IF EXISTS immut_no_update ON workflow_operator_audit", true},
		{"INSERT INTO customer(id) VALUES ('C1')", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isSupportedSchemaStatement(tt.statement); got != tt.want {
			t.Errorf("isSupportedSchemaStatement(%q) = %t, want %t", tt.statement, got, tt.want)
		}
	}
}

func isSupportedSchemaStatement(statement string) bool {
	fields := strings.Fields(statement)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "CREATE", "ALTER", "DROP":
		return true
	default:
		return false
	}
}

func TestSplitStatements_CustPaySchemas(t *testing.T) {
	for _, name := range []string{"cust_db.sql", "pay_db.sql"} {
		// Level 3 returns to templates/bank/ (the CWD of go test is the package directory internal/platform/migrate/).
		sql, err := os.ReadFile("../../../db/migrations/" + name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		stmts := SplitStatements(string(sql))
		if len(stmts) == 0 {
			t.Errorf("%s 切分后无语句", name)
		}
		for _, s := range stmts {
			if !isSupportedSchemaStatement(s) {
				t.Errorf("%s 含非 DDL 语句: %q", name, s)
			}
		}
	}
}

func TestSplitStatements_RewardRiskSchemas(t *testing.T) {
	for _, name := range []string{"reward_db.sql", "risk_db.sql"} {
		// Level 3 returns to templates/bank/ (the CWD of go test is the package directory internal/platform/migrate/).
		sql, err := os.ReadFile("../../../db/migrations/" + name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		stmts := SplitStatements(string(sql))
		if len(stmts) == 0 {
			t.Errorf("%s 切分后无语句", name)
		}
		for _, s := range stmts {
			if !isSupportedSchemaStatement(s) {
				t.Errorf("%s 含非 DDL 语句: %q", name, s)
			}
		}
	}
}

func TestSplitStatements_LoanWealthSchemas(t *testing.T) {
	for _, name := range []string{"loan_db.sql", "wealth_db.sql"} {
		// Level 3 returns to templates/bank/ (the CWD of go test is the package directory internal/platform/migrate/).
		sql, err := os.ReadFile("../../../db/migrations/" + name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		stmts := SplitStatements(string(sql))
		if len(stmts) == 0 {
			t.Errorf("%s 切分后无语句", name)
		}
		for _, s := range stmts {
			if !isSupportedSchemaStatement(s) {
				t.Errorf("%s 含非 DDL 语句: %q", name, s)
			}
		}
	}
}
