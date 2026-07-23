package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopy_CreatesFullProject(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := Copy("bank", r, dir, false); err != nil {
		t.Fatal(err)
	}
	must := []string{
		"go.mod", "go.sum", "docker-compose.yaml", "Dockerfile",
		"template.yaml", ".env.example", "Makefile",
		"README.md", "ARCHITECTURE.md",
		"cmd/core-banking/main.go", "cmd/seed/main.go",
		"db/migrations/core_db.sql",
		"internal/corebanking/domain/money.go",
		"internal/corebanking/service/ledger_service.go",
		"internal/fixtures/domains/core.go",
	}
	for _, p := range must {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s is missing after copy: %v", p, err)
		}
	}
}

func TestCopy_RejectsNonEmpty(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Copy("bank", r, dir, false); err != ErrDirNotEmpty {
		t.Errorf("a non-empty directory should return ErrDirNotEmpty, got %v", err)
	}
}

func TestCopy_ForceAllowsNonEmpty(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Copy("bank", r, dir, true); err != nil {
		t.Errorf("force should allow a non-empty directory: %v", err)
	}
}

func TestCopy_IsVerbatim(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := Copy("bank", r, dir, false); err != nil {
		t.Fatal(err)
	}
	want, err := readTarFile("bank/db/migrations/core_db.sql")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "db/migrations/core_db.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("the copied content should be byte-identical")
	}
}
