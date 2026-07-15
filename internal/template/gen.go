package template

// go:generate 把仓根 templates/bank 打包到本包目录的 templates.tar。
// 工作目录为本包目录（internal/template/），故 -C 指向 ../../templates。
// 改动 templates/bank 后须重新 `go generate ./internal/template`。
//
//go:generate tar -C ../../templates -cf templates.tar bank
