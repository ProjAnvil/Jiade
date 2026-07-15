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
		Short: "从模板拷贝出一个工程（逐字拷贝，零替换）",
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
			u.Step("拷贝模板 %s → %s", tplName, dir)
			if err := template.Copy(tplName, reg, dir, force); err != nil {
				return err
			}
			u.OK("完成。下一步：cd %s && jiade up && jiade seed", dir)
			return nil
		},
	}
	cmd.Flags().String("template", "", "模板名（如 bank）")
	cmd.Flags().Bool("force", false, "目标目录非空时强制覆盖")
	return cmd
}

func promptTemplate(reg *template.Registry) (string, error) {
	names, err := reg.Names()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("无可用模板")
	}
	sel := promptui.Select{Label: "选择模板", Items: names}
	_, result, err := sel.Run()
	return result, err
}

func promptDir() (string, error) {
	p := promptui.Prompt{
		Label: "目标目录（工程根，文件直接落在此目录下）",
		Validate: func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("不能为空")
			}
			return nil
		},
	}
	return p.Run()
}
