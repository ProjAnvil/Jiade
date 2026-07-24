package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCLIGenerateWritesDevSummary confirms the CLI produces the dev-scale
// summary on stdout without touching a database. This is the smoke test the
// spec mandates for the no-PostgreSQL path.
func TestCLIGenerateWritesDevSummary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"generate", "--scale", "dev", "--seed", "42"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`"scale": "dev"`,
		`"products": 80`,
		`"orders": 100`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\noutput:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "skipping database load") {
		t.Errorf("stderr should note the skip; got:\n%s", stderr.String())
	}
}

// TestCLIVerifyRejectsUnknownScale confirms bad scale values surface as errors
// rather than producing a misleading empty summary.
func TestCLIVerifyRejectsUnknownScale(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"generate", "--scale", "huge"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown scale, got nil")
	}
	if !strings.Contains(err.Error(), "huge") {
		t.Errorf("error should mention the bad scale: %v", err)
	}
}

// TestCLIUsageExitsCleanly confirms the help surface lists both subcommands and
// the determinism flag.
func TestCLIUsageExitsCleanly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("usage: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"generate", "verify", "--scale", "--seed", "--reset"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}
