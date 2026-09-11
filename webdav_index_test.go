package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/webdav"
)

type fakeUploadBackend struct {
	objects map[string][]byte
	cleared map[string]bool
}

func newFakeUploadBackend(initial map[string][]byte) *fakeUploadBackend {
	objects := make(map[string][]byte)
	for k, v := range initial {
		objects[k] = v
	}
	return &fakeUploadBackend{objects: objects, cleared: make(map[string]bool)}
}

func (b *fakeUploadBackend) PutObject(ctx context.Context, remotePath string, reader io.Reader) (*obsUploadResult, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	b.objects[remotePath] = data
	b.cleared[remotePath] = false
	return &obsUploadResult{
		FileURL:   "https://cdn/" + remotePath,
		SourceURL: "https://obs/" + remotePath,
		Key:       obsObjectRoot + "/" + remotePath,
	}, nil
}

func (b *fakeUploadBackend) ClearObject(ctx context.Context, remotePath string) error {
	b.objects[remotePath] = []byte{}
	b.cleared[remotePath] = true
	return nil
}

func newTestIndex(t *testing.T, backend uploadBackend) *webDAVIndex {
	t.Helper()
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	cacheDir := filepath.Join(dir, "cache")
	idx, err := newWebDAVIndex(indexPath, cacheDir, backend, nil, "tester")
	if err != nil {
		t.Fatalf("newWebDAVIndex: %v", err)
	}
	return idx
}

// newTestProdFS 构建多用户 FS：返回带凭据上下文 ctx(对应 testfs 用户) 与对应索引。
// 生产 indexedWebDAVFS 从 ctx 解析用户，因此这些直接调用需要 ctx 携带凭据。
func newTestProdFS(t *testing.T, backend uploadBackend, username, password string) (indexedWebDAVFS, context.Context, *webDAVIndex) {
	t.Helper()
	base := t.TempDir()
	store := newUserIndexes(base, backend)
	idx, err := store.indexFor(username, password)
	if err != nil {
		t.Fatalf("indexFor: %v", err)
	}
	ctx := withCreds(context.Background(), username, password)
	return indexedWebDAVFS{store: store}, ctx, idx
}

func TestCleanWebDAVPath(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"", "/", false},
		{"/", "/", false},
		{"/a/b.txt", "/a/b.txt", false},
		{"a//b", "/a/b", false},
		{"../x", "", true},
		{"/a/../x", "", true},
		{"中文 文件.txt", "/中文 文件.txt", false},
	}
	for _, c := range cases {
		got, err := cleanWebDAVPath(c.in)
		if c.err {
			if err == nil {
				t.Errorf("cleanWebDAVPath(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("cleanWebDAVPath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestRemotePathFromDAVPath(t *testing.T) {
	got, err := remotePathFromDAVPath("/a/b.txt")
	if err != nil || got != "a/b.txt" {
		t.Errorf("remotePathFromDAVPath(/a/b.txt) = %q, %v", got, err)
	}
	if _, err := remotePathFromDAVPath("/"); err == nil {
		t.Error("root should have no remote path")
	}
}

// TestFlatRemoteKey 验证摊平后的 key：无 "/"，含 username+路径+时间戳+.zip。
func TestFlatRemoteKey(t *testing.T) {
	key := flatRemoteKey("alice", "docs/report.txt")
	if strings.Contains(key, "/") {
		t.Errorf("扁平 key 不应含 /: %q", key)
	}
	if !strings.HasPrefix(key, "alice-docs-report.txt-") {
		t.Errorf("扁平 key 应形如 username-路径(连字符)-时间戳-: %q", key)
	}
	if !strings.HasSuffix(key, ".zip") {
		t.Errorf("扁平 key 应以 .zip 结尾: %q", key)
	}
	if len(key) <= len("alice-docs-report.txt-"+"0000.zip") {
		t.Error("应带时间戳")
	}
}

func TestMkdirAndPropfind(t *testing.T) {
	idx := newTestIndex(t, newFakeUploadBackend(nil))

	if err := idx.mkdir("/a"); err != nil {
		t.Fatalf("mkdir /a: %v", err)
	}
	if err := idx.mkdir("/a/b"); err != nil {
		t.Fatalf("mkdir /a/b: %v", err)
	}
	if err := idx.mkdir("/a"); err == nil {
		t.Error("mkdir existing should fail")
	}
	if err := idx.mkdir("/nonexistent/x"); err == nil {
		t.Error("mkdir under missing parent should fail")
	}

	children, err := idx.listChildren("/a")
	if err != nil {
		t.Fatalf("listChildren: %v", err)
	}
	if len(children) != 1 || children[0].Name() != "b" {
		t.Errorf("listChildren(/a) = %v", children)
	}

	if err := idx.removeAll(context.Background(), "/"); err == nil {
		t.Error("删除根目录应该失败")
	}
}

func TestPutGetDelete(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	fs, ctx, idx := newTestProdFS(t, backend, "tester", "pw")

	if err := fs.Mkdir(ctx, "/dir", 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// PUT via OpenFile writing path
	f, err := fs.OpenFile(ctx, "/dir/hello.txt", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("open write: %v", err)
	}
	if _, err := f.Write([]byte("hello world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close (commit upload): %v", err)
	}

	entry, err := idx.entry("/dir/hello.txt")
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	flatKey := entry.RemotePath
	if strings.Contains(flatKey, "/") {
		t.Errorf("扁平 key 不应含 /: %q", flatKey)
	}
	if !strings.HasPrefix(flatKey, "tester-dir-hello.txt-") {
		t.Errorf("扁平 key 应含 username+路径+时间戳: %q", flatKey)
	}
	if got := backend.objects[flatKey]; bytes.Contains(got, []byte("hello world")) {
		t.Errorf("OBS 对象应为密文，不应含明文 %q: %q", flatKey, got)
	}

	// GET (read from cache)
	rf, err := fs.OpenFile(ctx, "/dir/hello.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rf.Close()
	if string(got) != "hello world" {
		t.Errorf("read back = %q", got)
	}

	// DELETE
	if err := fs.RemoveAll(ctx, "/dir/hello.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !backend.cleared[flatKey] {
		t.Errorf("expected clearRemoteObject to be called on %q", flatKey)
	}
	if _, err := fs.Stat(ctx, "/dir/hello.txt"); err == nil {
		t.Error("deleted file should NotExist")
	}
}

func TestMoveAndCopy(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	fs, ctx, idx := newTestProdFS(t, backend, "tester", "pw")

	if err := fs.Mkdir(ctx, "/src", 0755); err != nil {
		t.Fatal(err)
	}
	f, err := fs.OpenFile(ctx, "/src/a.txt", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// MOVE
	if err := fs.Rename(ctx, "/src/a.txt", "/src/b.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	moved, err := idx.entry("/src/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := backend.objects[moved.RemotePath]; bytes.Contains(got, []byte("data")) {
		t.Errorf("move 上传对象应为密文: %q", moved.RemotePath)
	}
	if _, err := fs.Stat(ctx, "/src/a.txt"); err == nil {
		t.Error("old path should be gone")
	}
	if _, err := fs.Stat(ctx, "/src/b.txt"); err != nil {
		t.Error("new path should exist")
	}
}

func TestHTTPWebDAVServer(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	fs, _, idx := newTestProdFS(t, backend, "tester", "pw")

	origin := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
	}
	handler := authMiddleware(origin)

	if err := idx.mkdir("/folder"); err != nil {
		t.Fatal(err)
	}

	do := func(method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/dav"+path, body)
		req.SetBasicAuth("tester", "pw")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := do("PROPFIND", "/folder", nil, map[string]string{"Depth": "0"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusMultiStatus && rec.Code != 207 {
		t.Errorf("PROPFIND status = %d", rec.Code)
	}

	rec = do("PUT", "/folder/foo.txt", strings.NewReader("hi"), nil)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent {
		t.Errorf("PUT status = %d", rec.Code)
	}

	rec = do("GET", "/folder/foo.txt", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Errorf("GET status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = do("DELETE", "/folder/foo.txt", nil, nil)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Errorf("DELETE status = %d", rec.Code)
	}
}

func TestBasicAuth(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	h := authMiddleware(inner)

	req := httptest.NewRequest("GET", "/x", nil)
	req.SetBasicAuth("alice", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("auth ok status=%d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/x", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("no auth status=%d, want 401", rec2.Code)
	}

	// 空密码应拒绝
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, mustReqWithBasic("GET", "/x", "alice", ""))
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("empty password status=%d, want 401", rec3.Code)
	}
}

func mustReqWithBasic(method, path, user, pass string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.SetBasicAuth(user, pass)
	return req
}

func TestIndexPersistence(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	cacheDir := filepath.Join(dir, "cache")

	idx1, err := newWebDAVIndex(indexPath, cacheDir, backend, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx1.mkdir("/persist"); err != nil {
		t.Fatal(err)
	}

	idx2, err := newWebDAVIndex(indexPath, cacheDir, backend, nil, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx2.stat("/persist"); err != nil {
		t.Errorf("persisted dir missing after reload: %v", err)
	}
}

// TestEncryptedPutGet — 加密路径：上传内容在 OBS 是密文，GET 能解回明文。
func TestEncryptedPutGet(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	fs, ctx, idx := newTestProdFS(t, backend, "alice", "secret")

	if err := fs.Mkdir(ctx, "/dir", 0755); err != nil {
		t.Fatal(err)
	}

	// PUT via OpenFile write
	w, err := fs.OpenFile(ctx, "/dir/secret.txt", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("topsecret data")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// OBS 对象必须是密文（不包含明文内容）
	entry, err := idx.entry("/dir/secret.txt")
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	flatKey := entry.RemotePath
	if strings.Contains(flatKey, "/") {
		t.Errorf("扁平 key 不应含 /: %q", flatKey)
	}
	remote := backend.objects[flatKey]
	if bytes.Contains(remote, []byte("topsecret data")) {
		t.Error("OBS 对象不应包含明文内容")
	}
	// 本地缓存也应是密文
	cacheName := cacheNameForPath("/dir/secret.txt")
	cacheBytes, err := os.ReadFile(filepath.Join(idx.cacheDir, cacheName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cacheBytes, []byte("topsecret data")) {
		t.Error("本地缓存不应是明文")
	}

	// GET 能解回明文
	r, err := fs.OpenFile(ctx, "/dir/secret.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	r.Close()
	if string(got) != "topsecret data" {
		t.Errorf("read back = %q, want topsecret data", got)
	}

	// 合法删除后远端密文清空
	if err := fs.RemoveAll(ctx, "/dir/secret.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(ctx, "/dir/secret.txt"); err == nil {
		t.Error("deleted file should NotExist")
	}
}

// TestEncryptedIndexIsCiphertext 验证索引文件落盘是密文，不是明文 JSON。
func TestEncryptedIndexIsCiphertext(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	dek := deriveKey("alice", "secret")
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.json")
	cacheDir := filepath.Join(dir, "cache")
	idx, err := newWebDAVIndex(indexPath, cacheDir, backend, dek, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.mkdir("/abc"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("abcdef")) || bytes.Contains(raw, []byte("entries")) {
		t.Error("索引文件不应是明文 JSON")
	}
}

// TestMultiUserIsolation 单服务器多用户：不同账号登录彼此隔离，各自索引/密钥独立。
func TestMultiUserIsolation(t *testing.T) {
	backend := newFakeUploadBackend(nil)
	store := newUserIndexes(t.TempDir(), backend)
	fs := indexedWebDAVFS{store: store}

	origin := &webdav.Handler{Prefix: "/dav", FileSystem: fs, LockSystem: webdav.NewMemLS()}
	handler := authMiddleware(origin)

	do := func(user, pass, method, path string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/dav"+path, body)
		req.SetBasicAuth(user, pass)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// alice 上传一个文件
	if rec := do("alice", "pw", "PUT", "/sol.txt", strings.NewReader("alice secret file")); rec.Code >= 300 {
		t.Fatalf("alice PUT status=%d", rec.Code)
	}

	// alice 能读到
	if rec := do("alice", "pw", "GET", "/sol.txt", nil); rec.Code != http.StatusOK || rec.Body.String() != "alice secret file" {
		t.Fatalf("alice GET status=%d body=%q", rec.Code, rec.Body.String())
	}

	// bob（不同账号）看不到 alice 的文件：GET 应 404，列表里也没有
	if rec := do("bob", "pw", "GET", "/sol.txt", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("bob GET alice 的文件应 404, got %d", rec.Code)
	}

	// bob 用自己的账号上传同路径，互不覆盖 alice 的索引
	if rec := do("bob", "pw", "PUT", "/sol.txt", strings.NewReader("bob own")); rec.Code >= 300 {
		t.Fatalf("bob PUT status=%d", rec.Code)
	}
	if rec := do("bob", "pw", "GET", "/sol.txt", nil); rec.Body.String() != "bob own" {
		t.Fatalf("bob GET=%q", rec.Body.String())
	}
	if rec := do("alice", "pw", "GET", "/sol.txt", nil); rec.Body.String() != "alice secret file" {
		t.Fatalf("alice 数据被 bob 影响? GET=%q", rec.Body.String())
	}
}
