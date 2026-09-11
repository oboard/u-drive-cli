package main

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// pathutil.go — WebDAV 路径与凭据上下文工具。
// 从 webdav_index.go 抽出的小型复用函数。

type credsCtxKey struct{}

func withCreds(ctx context.Context, username, password string) context.Context {
	return context.WithValue(ctx, credsCtxKey{}, [2]string{username, password})
}

func credsFromCtx(ctx context.Context) (string, string, bool) {
	v, ok := ctx.Value(credsCtxKey{}).([2]string)
	if !ok {
		return "", "", false
	}
	return v[0], v[1], true
}

func cleanWebDAVPath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.Contains(name, "\x00") {
		return "", fmt.Errorf("路径包含非法字符")
	}
	if name == "" {
		return "/", nil
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("路径不能包含 ..")
		}
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if cleaned == "." {
		return "/", nil
	}
	return cleaned, nil
}

func parentWebDAVPath(name string) string {
	if name == "/" {
		return "/"
	}
	parent := path.Dir(name)
	if parent == "." {
		return "/"
	}
	return parent
}

func sanitizeUserDir(username string) string {
	u := strings.ReplaceAll(username, "\\", "/")
	u = strings.Trim(u, "/")
	if u == "" || u == "." || u == ".." {
		return "_default"
	}
	if strings.Contains(u, "..") {
		return "_default"
	}
	return u
}
