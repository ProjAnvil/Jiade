package cli

import (
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Options 持有跨子命令共享的全局状态。
type Options struct {
	Dir    string // 目标工程根目录（--dir）
	Stdout io.Writer
	Stderr io.Writer
}

// New 返回装配好子命令的 jiade root 命令。
func New() *cobra.Command {
	opts := &Options{Stdout: os.Stdout, Stderr: os.Stderr}
	root := &cobra.Command{
		Use:   "jiade",
		Short: "生成「现实世界大工程的缩影」——可运行的行业 Go 工程",
	}
	root.PersistentFlags().StringVar(&opts.Dir, "dir", "", "目标工程根目录（up/down/seed 默认当前目录；init 为空时交互式询问）")
	root.AddCommand(newListCmd(opts))
	root.AddCommand(newInitCmd(opts))
	root.AddCommand(newUpCmd(opts))
	root.AddCommand(newDownCmd(opts))
	root.AddCommand(newSeedCmd(opts))
	return root
}
