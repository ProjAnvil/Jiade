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
		Short: "在目标目录内 docker compose up -d（前置探测 docker）",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				dir = "." // 默认当前目录（配合 init 提示：cd <dir> 后再跑 jiade up/down）
			}
			build, _ := cmd.Flags().GetBool("build")
			u := ui.New(opts.Stdout, opts.Stderr)

			probe := docker.Probe(cmd.Context())
			if !probe.OK() {
				return fmt.Errorf("%s", probe.Hint())
			}
			u.Step("docker compose up（%s）", dir)
			upArgs := []string{"up", "-d"}
			if build {
				upArgs = append(upArgs, "--build")
			}
			return runCompose(opts.Stderr, dir, upArgs...)
		},
	}
	cmd.Flags().Bool("build", false, "compose up 时强制 --build")
	return cmd
}

func newDownCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "在目标目录内 docker compose down",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				dir = "." // 默认当前目录（配合 init 提示：cd <dir> 后再跑 jiade up/down）
			}
			ui.New(opts.Stdout, opts.Stderr).Step("docker compose down（%s）", dir)
			return runCompose(opts.Stderr, dir, "down")
		},
	}
}

// runCompose 在 dir 内执行 docker compose，stdout/stderr 透传，退出码透传。
func runCompose(stderr io.Writer, dir string, args ...string) error {
	c := exec.Command("docker", append([]string{"compose"}, args...)...)
	c.Dir = dir
	c.Stdout = os.Stdout
	c.Stderr = stderr
	return c.Run()
}
