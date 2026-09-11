package main

import (
	"fmt"
	"net/http"
	"path"

	"golang.org/x/net/webdav"
)

// webdav.go — WebDAV 服务器装配。
//
// 文件系统实现见 content_fs.go（基于 uLearning 内容 API）；
// 鉴权见 authMiddleware：Basic Auth 凭据即用户身份（派生根目录与加密密钥）。

func startWebDAVServer(addr, prefix string, noAuth bool) error {
	if prefix == "" {
		prefix = "/dav"
	} else {
		prefix = path.Clean("/" + prefix)
	}

	handler := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: newContentFileSystem(),
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				fmt.Printf("WebDAV %s %s: %v\n", r.Method, r.URL.Path, err)
			}
		},
	}

	var httpHandler http.Handler = handler
	if noAuth {
		// no-auth 模式：注入默认凭据
		httpHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := withCreds(r.Context(), "default", "default")
			handler.ServeHTTP(w, r.WithContext(ctx))
		})
	} else {
		httpHandler = authMiddleware(handler)
	}

	fmt.Printf("WebDAV 服务器启动在 http://%s%s\n", addr, prefix)
	fmt.Printf("目录树来自 uLearning 内容 API（多设备共享同一份远端数据）\n")
	if noAuth {
		fmt.Printf("警告：--no-auth 模式，使用默认凭据 default/default\n")
	} else {
		fmt.Printf("多用户：按每个请求的 Basic Auth 账号（username/password）派生根目录与加密密钥\n")
	}
	return http.ListenAndServe(addr, httpHandler)
}

// authMiddleware 校验请求带非空 Basic Auth，并把 username/password 注入 context，
// 供 FileSystem 解析对应用户的根目录与密钥（凭证即密钥，无需预配账号）。
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u == "" || p == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="udrive"`)
			http.Error(w, "未授权（需要用户名和密码）", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withCreds(r.Context(), u, p)))
	})
}
