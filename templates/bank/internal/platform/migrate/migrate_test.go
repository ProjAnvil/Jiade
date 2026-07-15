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

func TestSplitStatements_CustPaySchemas(t *testing.T) {
	for _, name := range []string{"cust_db.sql", "pay_db.sql"} {
		// 3 级回到 templates/bank/（go test 的 CWD 是包目录 internal/platform/migrate/）。
		sql, err := os.ReadFile("../../../db/migrations/" + name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		stmts := SplitStatements(string(sql))
		if len(stmts) == 0 {
			t.Errorf("%s 切分后无语句", name)
		}
		for _, s := range stmts {
			if !strings.Contains(s, "CREATE") {
				t.Errorf("%s 含非 DDL 语句: %q", name, s)
			}
		}
	}
}
