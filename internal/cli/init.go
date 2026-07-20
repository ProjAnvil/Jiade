package cli

import (
	"fmt"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/projanvil/jiade/internal/template"
	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newInitCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a project from a template (verbatim copy, no substitution)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tplName, _ := cmd.Flags().GetString("template")
			force, _ := cmd.Flags().GetBool("force")
			dir := opts.Dir

			reg, err := template.New()
			if err != nil {
				return err
			}
			u := ui.New(opts.Stdout, opts.Stderr)

			if tplName == "" {
				if tplName, err = promptTemplate(reg); err != nil {
					return err
				}
			}
			if dir == "" {
				if dir, err = promptDir(); err != nil {
					return err
				}
			}
			u.Step("Copy template %s -> %s", tplName, dir)
			if err := template.Copy(tplName, reg, dir, force); err != nil {
				return err
			}
			u.OK("Done. Next: cd %s && jiade up && jiade seed", dir)
			return nil
		},
	}
	cmd.Flags().String("template", "", "template name (e.g. bank)")
	cmd.Flags().Bool("force", false, "overwrite a non-empty target dir")
	return cmd
}

func promptTemplate(reg *template.Registry) (string, error) {
	names, err := reg.Names()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no templates available")
	}
	sel := promptui.Select{Label: "Select template", Items: names}
	_, result, err := sel.Run()
	return result, err
}

func promptDir() (string, error) {
	p := promptui.Prompt{
		Label: "Target dir (project root; files land directly here)",
		Validate: func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("cannot be empty")
			}
			return nil
		},
	}
	return p.Run()
}
