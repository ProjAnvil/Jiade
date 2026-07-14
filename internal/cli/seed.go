package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newSeedCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "运行目标工程的 fixture 生成器",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("seed: 尚未实现（见 Task 18）")
		},
	}
	cmd.Flags().String("scale", "dev", "规模：dev|full")
	cmd.Flags().Bool("reset", false, "重建库与表（幂等）")
	return cmd
}
