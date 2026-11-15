// Package web holds the embedded UI served by `vern serve`: the pages,
// scripts and styles under static/, and the example sample under examples/.
package web

import (
	"embed"
	"io"
	"io/fs"
	"sort"
	"strings"
)

//go:embed static examples/demo.jsonl.gz
var files embed.FS

// FS returns the UI's files with the contents of static/ at the root
// (index.html, app.js, ...) and the example at examples/demo.jsonl.gz.
func FS() fs.FS { return uiFS{} }

// uiFS overlays examples/ on static/. static/ must not contain an examples
// directory of its own.
type uiFS struct{}

func (uiFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	var (
		f   fs.File
		err error
	)
	switch {
	case name == ".":
		return openRoot()
	case name == "examples" || strings.HasPrefix(name, "examples/"):
		f, err = files.Open(name)
	default:
		f, err = files.Open("static/" + name)
	}
	if err != nil {
		// Report the caller's path, not the embedded one.
		return nil, &fs.PathError{Op: "open", Path: name, Err: unwrapPathError(err)}
	}
	return f, nil
}

func unwrapPathError(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}

// rootDir is "." : static/'s entries plus the examples directory.
type rootDir struct {
	fs.File // static/, for Close
	info    fs.FileInfo
	entries []fs.DirEntry
	offset  int
}

func openRoot() (fs.File, error) {
	f, err := files.Open("static")
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	entries, err := files.ReadDir("static")
	if err != nil {
		f.Close()
		return nil, err
	}
	top, err := files.ReadDir(".")
	if err != nil {
		f.Close()
		return nil, err
	}
	for _, e := range top {
		if e.Name() == "examples" {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return &rootDir{File: f, info: rootInfo{info}, entries: entries}, nil
}

func (d *rootDir) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *rootDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: ".", Err: fs.ErrInvalid}
}

// ReadDir follows fs.ReadDirFile: n <= 0 returns the rest, n > 0 pages.
func (d *rootDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.offset:]
	if n <= 0 {
		d.offset = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.offset += n
	return rest[:n], nil
}

// rootInfo renames static/'s FileInfo to ".".
type rootInfo struct{ fs.FileInfo }

func (rootInfo) Name() string { return "." }
