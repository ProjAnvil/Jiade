package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出可用模板",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("list: 尚未实现（见 Task 16）")
		},
	}
}
