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

	// Browser tab indicator. Served straight from the embedded asset
	// because the index.html <link rel="icon" href="/favicon.ico" />
	// resolves to the site root, not /assets/. Without an explicit
	// route the catch-all `/` handler below 404's it (since the path
	// is not exactly `/`). The same favicon.ico bytes also live at
	// cmd/mcphub/mcphub.ico for the Windows .exe icon — both are
	// generated from internal/branding by tools/genicon (single
	// source of truth for the visual identity).
	s.mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(sub, "favicon.ico")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
	})

	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		b, _ := fs.ReadFile(sub, "index.html")
		_, _ = w.Write(b)
	})
}
