package cli

import (
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Options holds state shared across subcommands.
type Options struct {
	Dir    string // target project root (--dir)
	Stdout io.Writer
	Stderr io.Writer
}

// New returns the jiade root command with subcommands wired up.
func New() *cobra.Command {
	opts := &Options{Stdout: os.Stdout, Stderr: os.Stderr}
	root := &cobra.Command{
		Use:   "jiade",
		Short: "Generate a microcosm of real-world large-scale engineering — a runnable industry Go project",
	}
	root.PersistentFlags().StringVar(&opts.Dir, "dir", "", "target project root (up/down/seed default to the current dir; init prompts if empty)")
	root.AddCommand(newListCmd(opts))
	root.AddCommand(newInitCmd(opts))
	root.AddCommand(newUpCmd(opts))
	root.AddCommand(newDownCmd(opts))
	root.AddCommand(newSeedCmd(opts))
	return root
}
