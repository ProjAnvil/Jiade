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
		Short: "Run the target project's fixture generator",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				dir = "." // default to CWD (matches the init hint: cd <dir>, then jiade seed)
			}
			scale, _ := cmd.Flags().GetString("scale")
			reset, _ := cmd.Flags().GetBool("reset")
			ui.New(opts.Stdout, opts.Stderr).Step("seed (%s, scale=%s reset=%v)", dir, scale, reset)

			c := exec.Command("go", "run", "./cmd/seed", "--scale="+scale)
			if reset {
				c.Args = append(c.Args, "--reset")
			}
			c.Dir = dir
			c.Stdout = os.Stdout
			c.Stderr = opts.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("%w (run 'jiade up' first to start postgres)", err)
			}
			return nil
		},
	}
	cmd.Flags().String("scale", "dev", "scale: dev|full")
	cmd.Flags().Bool("reset", false, "rebuild databases and tables (idempotent)")
	return cmd
}
