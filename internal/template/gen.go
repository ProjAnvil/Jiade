package template

// go:generate packages the bank root templates/bank into templates.tar in this package directory.
// The working directory is the package directory (internal/template/), so -C points to ../../templates.
// After changing templates/bank, you must re-`go generate ./internal/template`.
//
//go:generate tar -C ../../templates -cf templates.tar bank
