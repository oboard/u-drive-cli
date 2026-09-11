package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sync"

	"golang.org/x/net/webdav"
)

type indexedWebDAVFS struct {
	index *webDAVIndex
}

func (fs indexedWebDAVFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return fs.index.stat(name)
}

func (fs indexedWebDAVFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return fs.index.mkdir(name)
}

func (fs indexedWebDAVFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, err
	}

	entry, err := fs.index.entry(cleaned)
	if err == nil && entry.IsDir {
		return &indexDir{index: fs.index, name: cleaned}, nil
	}

	isWrite := flag&os.O_CREATE != 0 || flag&os.O_TRUNC != 0 || flag&os.O_RDWR != 0 || flag&os.O_WRONLY != 0
	if isWrite {
		return fs.index.openForWrite(cleaned, flag)
	}

	file, cleanupPath, _, err := fs.index.openCacheFile(cleaned)
	if err != nil {
		return nil, err
	}
	return &indexFile{index: fs.index, name: cleaned, file: file, cleanupPath: cleanupPath}, nil
}

func (fs indexedWebDAVFS) RemoveAll(ctx context.Context, name string) error {
	return fs.index.removeAll(ctx, name)
}

func (fs indexedWebDAVFS) Rename(ctx context.Context, oldName, newName string) error {
	return fs.index.rename(ctx, oldName, newName)
}

type indexDir struct {
	index *webDAVIndex
	name  string
}

func (d *indexDir) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("目录不支持读取")
}

func (d *indexDir) Seek(offset int64, whence int) (int64, error) {
	return 0, fmt.Errorf("目录不支持 Seek")
}

func (d *indexDir) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("目录不支持写入")
}

func (d *indexDir) Close() error { return nil }

func (d *indexDir) Readdir(count int) ([]os.FileInfo, error) {
	children, err := d.index.listChildren(d.name)
	if err != nil {
		return nil, err
	}
	if count <= 0 {
		return children, nil
	}
	if len(children) > count {
		return children[:count], nil
	}
	return children, io.EOF
}

func (d *indexDir) Stat() (os.FileInfo, error) {
	return d.index.stat(d.name)
}

type indexFile struct {
	mu          sync.Mutex
	index       *webDAVIndex
	name        string
	file        *os.File
	tempPath    string // 写提交用的临时文件（提交上传）
	cleanupPath string // 读解密产生的明文临时文件（Close 时清理）
}

func (f *indexFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Read(p)
}

func (f *indexFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Seek(offset, whence)
}

func (f *indexFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Write(p)
}

func (f *indexFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("不支持 Readdir，%s 是文件", f.name)
}

func (f *indexFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil, os.ErrClosed
	}
	return f.file.Stat()
}

func (f *indexFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return nil
	}
	file := f.file
	f.file = nil

	if f.tempPath != "" {
		err := f.index.commitUpload(context.Background(), f.name, f.tempPath, file)
		if err != nil {
			_ = os.Remove(f.tempPath)
			return err
		}
		return nil
	}

	closeErr := file.Close()
	if f.cleanupPath != "" {
		_ = os.Remove(f.cleanupPath)
	}
	return closeErr
}

func (idx *webDAVIndex) openForWrite(name string, flag int) (webdav.File, error) {
	tempFile, tempPath, cleaned, err := idx.prepareWrite(name, flag)
	if err != nil {
		return nil, err
	}
	return &indexFile{index: idx, name: cleaned, file: tempFile, tempPath: tempPath}, nil
}

func startWebDAVServer(username, addr, prefix, user, pass string, noAuth bool, indexPath, cacheDir string) error {
	backend := obsUploadBackend{}

	// 「凭证即密钥」：密钥由 username+password 纯函数派生，无需 keyring/备份。
	if username == "" {
		return fmt.Errorf("WebDAV 需要 --username（用于加密密钥派生与隔离）")
	}
	dek := deriveKey(username, pass)

	index, err := newWebDAVIndex(indexPath, cacheDir, backend, dek, username)
	if err != nil {
		return fmt.Errorf("初始化 WebDAV 索引失败: %v", err)
	}

	if prefix == "" {
		prefix = "/dav"
	} else {
		prefix = path.Clean("/" + prefix)
	}

	fs := indexedWebDAVFS{index: index}
	handler := &webdav.Handler{
		Prefix:     prefix,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				fmt.Printf("WebDAV %s %s: %v\n", r.Method, r.URL.Path, err)
			}
		},
	}

	var httpHandler http.Handler = handler
	if !noAuth {
		httpHandler = basicAuth(handler, user, pass)
	}

	fmt.Printf("WebDAV 服务器启动在 http://%s%s\n", addr, prefix)
	fmt.Printf("列表来自本地索引（upload-only hack，不代表远端真实内容）\n")
	return http.ListenAndServe(addr, httpHandler)
}

func basicAuth(next http.Handler, user, pass string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || !secureStringEqual(u, user) || !secureStringEqual(p, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="udrive"`)
			http.Error(w, "未授权", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
