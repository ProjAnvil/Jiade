package template

// go:generate packages all built-in templates into templates.tar.
// The working directory is the package directory (internal/template/), so -C points to ../../templates.
// After changing a template, you must re-`go generate ./internal/template`.
//
//go:generate tar -C ../../templates -cf templates.tar bank commerce
