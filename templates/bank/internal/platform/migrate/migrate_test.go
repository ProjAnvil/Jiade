package migrate

import (
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
