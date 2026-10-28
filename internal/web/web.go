// Package web serves the UI: a static page over the HTTP API.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Register serves the UI at /. The page holds no data: it reads the API with
// the token the user enters.
func Register(mux *http.ServeMux) {
	files, _ := fs.Sub(static, "static")
	server := http.FileServerFS(files)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		server.ServeHTTP(w, r)
	}))
}
