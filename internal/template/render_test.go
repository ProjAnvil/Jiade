package template

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestCopy_CommerceRetainsGatewayTelemetryAndObservabilityArtifacts(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := Copy("commerce", r, dir, false); err != nil {
		t.Fatal(err)
	}

	compose := readRenderedTemplateFile(t, dir, "compose.yaml")
	ingress := readRenderedTemplateFile(t, dir, "deploy/k8s/gateway.yaml")
	for name, config := range map[string]string{"compose": compose, "ingress": ingress} {
		if strings.Contains(config, "/internal/v1") {
			t.Fatalf("%s gateway config exposes an internal route", name)
		}
	}

	sharedConfig := readRenderedTemplateFile(t, dir, "deploy/k8s/config.yaml")
	for _, setting := range []string{
		"OTEL_ENABLED: \"false\"",
		"OTEL_EXPORTER_OTLP_ENDPOINT: otel-collector:4317",
		"OTEL_EXPORTER_OTLP_INSECURE: \"true\"",
	} {
		if !strings.Contains(sharedConfig, setting) {
			t.Fatalf("rendered Kubernetes shared config is missing %q", setting)
		}
	}

	dashboard := readRenderedTemplateFile(t, dir, "deploy/grafana/dashboards/commerce-overview.json")
	var decoded any
	if err := json.Unmarshal([]byte(dashboard), &decoded); err != nil {
		t.Fatalf("rendered dashboard is invalid JSON: %v", err)
	}
	for _, expression := range []string{
		"rate(http_requests_total[5m])",
		"histogram_quantile(0.95, sum by (le,service) (rate(http_request_duration_seconds_bucket[5m])))",
		"max by (service) (outbox_oldest_age_seconds)",
	} {
		if !strings.Contains(dashboard, expression) {
			t.Fatalf("rendered dashboard is missing query %q", expression)
		}
	}

	traceSmoke := filepath.Join(dir, "test", "trace-smoke.sh")
	if output, err := exec.Command("bash", "-n", traceSmoke).CombinedOutput(); err != nil {
		t.Fatalf("rendered trace smoke script is invalid: %v\n%s", err, output)
	}
}

func TestTemplateArchiveMatchesTemplateSources(t *testing.T) {
	want := make(map[string]templateSourceFile)
	sourceRoot := filepath.Join("..", "..", "templates")
	if err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want[filepath.ToSlash(relative)] = templateSourceFile{contents: contents, mode: info.Mode().Perm()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	archive := tar.NewReader(bytes.NewReader(templatesTar))
	got := make(map[string]templateSourceFile, len(want))
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		contents, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		got[header.Name] = templateSourceFile{contents: contents, mode: os.FileMode(header.Mode).Perm()}
	}
	if len(got) != len(want) {
		t.Fatalf("archive contains %d files, want %d source files; regenerate templates.tar", len(got), len(want))
	}
	for path, source := range want {
		archived, ok := got[path]
		if !ok {
			t.Fatalf("archive is missing %s; regenerate templates.tar", path)
		}
		if !bytes.Equal(archived.contents, source.contents) {
			t.Fatalf("archive content differs for %s; regenerate templates.tar", path)
		}
		if archived.mode != source.mode {
			t.Fatalf("archive mode for %s is %#o, want %#o", path, archived.mode, source.mode)
		}
	}
}

type templateSourceFile struct {
	contents []byte
	mode     os.FileMode
}

func readRenderedTemplateFile(t *testing.T, dir, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read rendered %s: %v", name, err)
	}
	return string(contents)
}
