package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestList_PrintsBank(t *testing.T) {
	opts := &Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	cmd := newListCmd(opts)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := opts.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "bank") {
		t.Errorf("list output should contain \"bank\": %q", out)
	}
}
