package main

import (
	"embed"
	"io/fs"
)

//go:embed static
var embeddedStatic embed.FS

var staticFS fs.FS = mustSub(embeddedStatic, "static")

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
