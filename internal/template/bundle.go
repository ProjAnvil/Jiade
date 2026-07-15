package template

import (
	"archive/tar"
	"bytes"
	_ "embed"
	"fmt"
	"io"
)

// templates.tar 由 go:generate 从仓根 templates/bank 打包而来（见 gen.go）。
// 单文件 embed：避开 go:embed 不能嵌入嵌套 module（bank/go.mod）的限制。
//
//go:embed templates.tar
var templatesTar []byte

// readTarFile 从内嵌 tar 读取指定路径的普通文件内容。
func readTarFile(name string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(templatesTar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("template: 读 tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Name == name {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("template: tar 中未找到 %s", name)
}
