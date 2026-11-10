// Package webui 单独提供 React Web Admin 的静态资源 handler。
//
// Web Admin 是可选的独立模块：独立后台按启动配置决定是否装配；
// CLI/MyGo 内嵌入口只用 API handler，永不挂载本模块。关闭 Web Admin
// 只影响网页和静态资源，不改变 API、消息总线或业务能力。
package webui

import (
	"io/fs"
	"net/http"

	kmweb "github.com/bsv8/keymaster.cc-cli/web"
)

// Handler serves the embedded React build (web/dist). The placeholder is
// only a fallback when the frontend was not built; production binaries always
// carry the built assets and need no Node.js at runtime.
//
// Single-page app: unknown non-API paths fall back to index.html. /api/*
// is never handled here (the API mux answers it first, including 404 for
// unknown API paths), so this handler must only be mounted at "/".
func Handler() http.Handler {
	sub, err := fs.Sub(kmweb.Dist, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.Header().Set("allow", "GET")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			http.Error(w, "web ui unavailable", http.StatusNotFound)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			if _, err := fs.Stat(sub, stripLeadingSlash(r.URL.Path)); err != nil {
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

func stripLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
