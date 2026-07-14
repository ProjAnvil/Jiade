package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "在目标目录内 docker compose up -d（前置探测 docker）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("up: 尚未实现（见 Task 18）")
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
			return fmt.Errorf("down: 尚未实现（见 Task 18）")
		},
	}
}
