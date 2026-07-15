// Package ui 提供 jiade 的终端输出（符号前缀，无颜色库依赖）。
package ui

import (
	"fmt"
	"io"
)

type UI struct {
	Out io.Writer
	Err io.Writer
}

func New(out, errw io.Writer) *UI {
	return &UI{Out: out, Err: errw}
}

func (u *UI) Step(format string, args ...any)  { fmt.Fprintf(u.Out, "▶ "+format+"\n", args...) }
func (u *UI) OK(format string, args ...any)    { fmt.Fprintf(u.Out, "✓ "+format+"\n", args...) }
func (u *UI) Warn(format string, args ...any)  { fmt.Fprintf(u.Err, "! "+format+"\n", args...) }
func (u *UI) Error(format string, args ...any) { fmt.Fprintf(u.Err, "✗ "+format+"\n", args...) }
