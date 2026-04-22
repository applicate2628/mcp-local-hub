// internal/gui/assets.go
package gui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets/*
var assetsFS embed.FS

func registerAssetRoutes(s *Server) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err) // embed.FS is a compile-time source, so this cannot fail at runtime
	}
	fileServer := http.FileServer(http.FS(sub))
	s.mux.Handle("/assets/", http.StripPrefix("/assets/", fileServer))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := fs.ReadFile(sub, "index.html")
		_, _ = w.Write(b)
	})
}
