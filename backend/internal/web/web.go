// Package web serves the frontend: the files of its build, embedded in the binary when the image
// is built (the Dockerfile copies frontend/dist to internal/web/dist before go build).
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Files returns the embedded build of the frontend, or nil when this binary was built without it.
func Files() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}

// csp allows the page only its own scripts, styles and API. Styles may be inline: React sets
// style attributes (the designer's canvas, menus' positions).
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// Handler serves the frontend from files: a file when the path names one, and index.html for any
// other page, which the router in the browser resolves. Built assets have content hashes in
// their names and are cached for a year; everything else is revalidated.
func Handler(files fs.FS) http.Handler {
	server := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if info, err := fs.Stat(files, name); err == nil && !info.IsDir() && name != "index.html" {
			if strings.HasPrefix(name, "assets/") {
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				h.Set("Cache-Control", "no-cache")
			}
			server.ServeHTTP(w, r)
			return
		}
		if path.Ext(name) != "" && name != "index.html" {
			// A missing file, such as an asset of an older release: not the page.
			http.NotFound(w, r)
			return
		}
		page, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h.Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(page))
	})
}
