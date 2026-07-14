package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newInitCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "从模板拷贝出一个工程（逐字拷贝，零替换）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("init: 尚未实现（见 Task 17）")
		},
	}
	cmd.Flags().String("template", "", "模板名（如 bank）")
	cmd.Flags().Bool("force", false, "目标目录非空时强制覆盖")
	return cmd
}
