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

// indexedWebDAVFS 从 request context 解析当前用户，委托到其专属索引。
type indexedWebDAVFS struct {
	store *userIndexes
}

func (fs indexedWebDAVFS) idx(ctx context.Context) (*webDAVIndex, error) {
	u, p, ok := credsFromCtx(ctx)
	if !ok {
		return nil, os.ErrPermission
	}
	return fs.store.indexFor(u, p)
}

func (fs indexedWebDAVFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	idx, err := fs.idx(ctx)
	if err != nil {
		return nil, err
	}
	return idx.stat(name)
}

func (fs indexedWebDAVFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	idx, err := fs.idx(ctx)
	if err != nil {
		return err
	}
	return idx.mkdir(name)
}

func (fs indexedWebDAVFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	idx, err := fs.idx(ctx)
	if err != nil {
		return nil, err
	}

	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, err
	}

	entry, err := idx.entry(cleaned)
	if err == nil && entry.IsDir {
		return &indexDir{index: idx, name: cleaned}, nil
	}

	isWrite := flag&os.O_CREATE != 0 || flag&os.O_TRUNC != 0 || flag&os.O_RDWR != 0 || flag&os.O_WRONLY != 0
	if isWrite {
		return idx.openForWrite(cleaned, flag)
	}

	file, cleanupPath, _, err := idx.openCacheFile(cleaned)
	if err != nil {
		return nil, err
	}
	return &indexFile{index: idx, name: cleaned, file: file, cleanupPath: cleanupPath}, nil
}

func (fs indexedWebDAVFS) RemoveAll(ctx context.Context, name string) error {
	idx, err := fs.idx(ctx)
	if err != nil {
		return err
	}
	return idx.removeAll(ctx, name)
}

func (fs indexedWebDAVFS) Rename(ctx context.Context, oldName, newName string) error {
	idx, err := fs.idx(ctx)
	if err != nil {
		return err
	}
	return idx.rename(ctx, oldName, newName)
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

func startWebDAVServer(addr, prefix string, noAuth bool, baseDir string) error {
	backend := obsUploadBackend{}

	if baseDir == "" {
		var err error
		baseDir, err = defaultDataDir()
		if err != nil {
			return err
		}
	}
	store := newUserIndexes(baseDir, backend)

	if prefix == "" {
		prefix = "/dav"
	} else {
		prefix = path.Clean("/" + prefix)
	}

	fs := indexedWebDAVFS{store: store}
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
		httpHandler = authMiddleware(handler)
	}

	fmt.Printf("WebDAV 服务器启动在 http://%s%s\n", addr, prefix)
	fmt.Printf("列表来自本地索引（upload-only hack，不代表远端真实内容）\n")
	fmt.Printf("多用户：按每个请求的 Basic Auth 账号（username/password）隔离加密空间\n")
	return http.ListenAndServe(addr, httpHandler)
}

// authMiddleware 校验请求带非空 Basic Auth，并把 username/password 注入 context，
// 供 FileSystem 解析对应用户的密钥与索引（凭证即密钥，无需预配账号）。
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
