//go:build ignore

// pack generates a deterministic templates.tar archive from the
// templates/bank and templates/commerce directories.
//
// Determinism is achieved by sorting entries by path and using fixed
// metadata (mtime=Unix epoch, uid=0, gid=0, uniform mode 0644). This
// replaces the previous `tar -cf` invocation, which produced different
// output on macOS (bsdtar) and Linux (GNU tar) and made CI's
// `git diff --exit-code` check fail when developers committed a tar
// generated on macOS.
//
// Usage from internal/template/:
//
//	go run pack.go
package main

import (
	"archive/tar"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	templateRoot = "../../templates"
	output       = "templates.tar"
)

func main() {
	var paths []string
	err := filepath.WalkDir(templateRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(templateRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Only pack the bank and commerce subdirectories.
		if !strings.HasPrefix(rel, "bank/") && !strings.HasPrefix(rel, "commerce/") {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk %s: %v\n", templateRoot, err)
		os.Exit(1)
	}
	sort.Strings(paths)

	f, err := os.Create(output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", output, err)
		os.Exit(1)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	epoch := time.Unix(0, 0)
	for _, rel := range paths {
		full := filepath.Join(templateRoot, rel)
		data, err := os.ReadFile(full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", rel, err)
			os.Exit(1)
		}
		// Preserve source file permissions so TestTemplateArchiveMatchesTemplateSources
		// (which compares perm bits) passes. Git tracks only 0644/0755, so this
		// stays deterministic across macOS and Linux.
		info, err := os.Stat(full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stat %s: %v\n", rel, err)
			os.Exit(1)
		}
		hdr := &tar.Header{
			Name:     rel,
			Mode:     int64(info.Mode().Perm()),
			Size:     int64(len(data)),
			ModTime:  epoch,
			Typeflag: tar.TypeReg,
			Format:   tar.FormatGNU,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			fmt.Fprintf(os.Stderr, "write header %s: %v\n", rel, err)
			os.Exit(1)
		}
		if _, err := tw.Write(data); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", rel, err)
			os.Exit(1)
		}
	}
	fmt.Printf("packed %d files into %s\n", len(paths), output)
}
