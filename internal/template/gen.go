package template

// go:generate packages all built-in templates into templates.tar.
// The working directory is the package directory (internal/template/), so -C points to ../../templates.
// After changing a template, you must re-`go generate ./internal/template`.
//
// COPYFILE_DISABLE=1 prevents macOS bsdtar from embedding com.apple.* extended
// attributes (e.g. com.apple.provenance) as extra archive entries, which would
// inflate the file count and break TestTemplateArchiveMatchesTemplateSources.
//go:generate env COPYFILE_DISABLE=1 tar -C ../../templates -cf templates.tar bank commerce
