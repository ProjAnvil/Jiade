package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/projanvil/jiade/internal/docker"
	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run docker compose up -d in the target dir (probes docker first)",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				dir = "." // default to CWD (matches the init hint: cd <dir>, then jiade up/down)
			}
			build, _ := cmd.Flags().GetBool("build")
			u := ui.New(opts.Stdout, opts.Stderr)

			probe := docker.Probe(cmd.Context())
			if !probe.OK() {
				return fmt.Errorf("%s", probe.Hint())
			}
			u.Step("docker compose up (%s)", dir)
			upArgs := []string{"up", "-d"}
			if build {
				upArgs = append(upArgs, "--build")
			}
			return runCompose(opts.Stderr, dir, upArgs...)
		},
	}
	cmd.Flags().Bool("build", false, "force --build on compose up")
	return cmd
}

func newDownCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Run docker compose down in the target dir",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				dir = "." // default to CWD (matches the init hint: cd <dir>, then jiade up/down)
			}
			ui.New(opts.Stdout, opts.Stderr).Step("docker compose down (%s)", dir)
			return runCompose(opts.Stderr, dir, "down")
		},
	}
}

// runCompose runs docker compose inside dir; stdout/stderr and the exit code are passed through.
func runCompose(stderr io.Writer, dir string, args ...string) error {
	c := exec.Command("docker", append([]string{"compose"}, args...)...)
	c.Dir = dir
	c.Stdout = os.Stdout
	c.Stderr = stderr
	return c.Run()
}
