package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// seedManifest is the subset of template.yaml read by the seed command.
// The manifest is copied verbatim with the project (see internal/template).
type seedManifest struct {
	Seed struct {
		Entrypoint string `yaml:"entrypoint"`
	} `yaml:"seed"`
}

// seedCommand resolves the fixture-generator command for dir: the
// seed.entrypoint from dir/template.yaml when present (layouts differ per
// template, e.g. bank_dcn keeps it under services/seed/), otherwise the
// conventional ./cmd/seed.
func seedCommand(dir string) []string {
	data, err := os.ReadFile(dir + string(os.PathSeparator) + "template.yaml")
	if err == nil {
		var m seedManifest
		if err := yaml.Unmarshal(data, &m); err == nil && m.Seed.Entrypoint != "" {
			if fields := strings.Fields(m.Seed.Entrypoint); len(fields) > 0 {
				return fields
			}
		}
	}
	return []string{"go", "run", "./cmd/seed"}
}

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

			argv := seedCommand(dir)
			c := exec.Command(argv[0], argv[1:]...)
			c.Args = append(c.Args, "--scale="+scale)
			if reset {
				c.Args = append(c.Args, "--reset")
			}
			c.Dir = dir
			c.Stdout = os.Stdout
			c.Stderr = opts.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("%w (run 'jiade up' first to start the databases)", err)
			}
			return nil
		},
	}
	cmd.Flags().String("scale", "dev", "scale: dev|full")
	cmd.Flags().Bool("reset", false, "rebuild databases and tables (idempotent)")
	return cmd
}
