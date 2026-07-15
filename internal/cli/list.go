package cli

import (
	"github.com/projanvil/jiade/internal/template"
	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出可用模板",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := template.New()
			if err != nil {
				return err
			}
			names, err := reg.Names()
			if err != nil {
				return err
			}
			u := ui.New(opts.Stdout, opts.Stderr)
			if len(names) == 0 {
				u.Warn("无可用模板")
				return nil
			}
			for _, n := range names {
				desc := ""
				if m, err := reg.Manifest(n); err == nil {
					desc = m.Description
				}
				u.Step("%s — %s", n, desc)
			}
			return nil
		},
	}
}
