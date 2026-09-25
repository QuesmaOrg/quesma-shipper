package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
)

// contentTypes pins what the embedded assets are served as. http.FileServer would otherwise ask the
// operating system, and macOS answers "application/javascript" for .js where Linux answers
// "text/javascript" -- so the type a browser saw depended on the host the binary ran on, and the
// test asserting it passed in CI while failing on a developer's machine.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
	".png":   "image/png",
	".svg":   "image/svg+xml",
}

// adminUIHandler serves a self-contained UI with no runtime asset dependencies.
func adminUIHandler() http.Handler {
	assets, err := fs.Sub(adminUIAssets, "admin-ui")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/admin/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if kind, ok := contentTypes[path.Ext(r.URL.Path)]; ok {
			// Set before ServeHTTP: FileServer only sniffs when the header is absent.
			w.Header().Set("Content-Type", kind)
		}
		files.ServeHTTP(w, r)
	})
}

//go:embed admin-ui/*
var adminUIAssets embed.FS
