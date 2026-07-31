package template

// gen.go holds the generate directive that packs all built-in templates
// into templates.tar.  The working directory is the package directory
// (internal/template/), so pack.go resolves ../../templates relative to
// here.  After changing a template, you must re-`go generate ./internal/template`.
//
// pack.go replaces the previous `tar -cf` invocation, which produced
// different output on macOS (bsdtar) and Linux (GNU tar). The Go-based
// packer is fully deterministic (fixed mtime/uid/gid/mode, sorted entries).
//
//go:generate go run pack.go
