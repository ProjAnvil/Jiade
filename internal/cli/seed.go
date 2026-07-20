package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newSeedCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "运行目标工程的 fixture 生成器",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				dir = "." // 默认当前目录（配合 init 提示：cd <dir> 后再跑 jiade seed）
			}
			scale, _ := cmd.Flags().GetString("scale")
			reset, _ := cmd.Flags().GetBool("reset")
			ui.New(opts.Stdout, opts.Stderr).Step("seed（%s, scale=%s reset=%v）", dir, scale, reset)

			c := exec.Command("go", "run", "./cmd/seed", "--scale="+scale)
			if reset {
				c.Args = append(c.Args, "--reset")
			}
			c.Dir = dir
			c.Stdout = os.Stdout
			c.Stderr = opts.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("%w（请先 jiade up 启动 postgres）", err)
			}
			return nil
		},
	}
	cmd.Flags().String("scale", "dev", "规模：dev|full")
	cmd.Flags().Bool("reset", false, "重建库与表（幂等）")
	return cmd
}
