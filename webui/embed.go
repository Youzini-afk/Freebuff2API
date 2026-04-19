package webui

import (
	"embed"
	"io/fs"
)

//go:embed index.html
var files embed.FS

// FS returns the embedded WebUI filesystem rooted at the package's static
// assets directory.
func FS() fs.FS {
	return files
}
