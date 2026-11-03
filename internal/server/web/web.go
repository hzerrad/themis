// Package web serves the UI: a static page over the HTTP API.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Register serves the UI's files only, so every other path is the API's to
// answer. The page holds no data: it reads the API with the token the user
// enters.
func Register(mux *http.ServeMux) {
	files, _ := fs.Sub(static, "static")
	server := http.FileServerFS(files)
	tags := etags(files)
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Embedded files have no modification time, so without these a
		// browser keeps an old page after an upgrade. It asks each time, and
		// the tag makes an unchanged file cost one short reply.
		w.Header().Set("Cache-Control", "no-cache")
		if tag, ok := tags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
		}
		server.ServeHTTP(w, r)
	})
	for _, path := range []string{"/{$}", "/app.js", "/style.css"} {
		mux.Handle("GET "+path, page)
	}
}

// etags tags each file by its content, by the path it's served at.
func etags(files fs.FS) map[string]string {
	tags := map[string]string{}
	for path, name := range map[string]string{"/": "index.html", "/app.js": "app.js", "/style.css": "style.css"} {
		data, err := fs.ReadFile(files, name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		tags[path] = `"` + hex.EncodeToString(sum[:8]) + `"`
	}
	return tags
}
