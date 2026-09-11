package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// content_fs_test.go — contentFileSystem 的单元测试。
//
// 三个测试假件：
//   1. fakeContentAPI    — 内存树实现 apiClient（目录/文件记录）。
//   2. fakeUploadBackend — 实现 uploadBackend，把密文对象存在内存 map，
//                          并挂一个 httptest.Server 供 openRemoteContentFile 的 http.Get 拉取。
//   3. newTestFS         — 组装 sessionManager + fakes，返回 fs 与带凭据的 ctx。
//
// 注意 contentSession 有 15s 的 children 缓存；fs 的 Mkdir/RemoveAll/Rename/Close
// 会对受影响的 parent 调 invalidate，因此经由 fs 的「写后读」不受缓存影响；
// 绕过 fs 的直接 fake 变更则需要新建 manager（见 TestMultiUserIsolation）。
// deriveKey 是 PBKDF2（100k 迭代），每个不同账号组合约几十毫秒，
// 测试里避免不必要的新账号组合。

// ---------------------------------------------------------------- fake apiClient

// fakeContentAPI 是内存版 uLearning content API。
// children: parentID -> 该父目录下的记录（保持插入顺序）。
type fakeContentAPI struct {
	mu           sync.Mutex
	nextID       int64
	children     map[int64][]contentFileInfo
	createdFiles []uploadContentRecord // 每次成功 CreateFile 的记录（含 Location）
}

func newFakeContentAPI() *fakeContentAPI {
	return &fakeContentAPI{
		nextID:   1,
		children: make(map[int64][]contentFileInfo),
	}
}

func (f *fakeContentAPI) nextContentID() int64 {
	id := f.nextID
	f.nextID++
	return id
}

func (f *fakeContentAPI) ListChildren(parentID int64) ([]contentFileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.children[parentID]
	out := make([]contentFileInfo, 0, len(list))
	out = append(out, list...)
	return out, nil
}

func (f *fakeContentAPI) GetMeta(id int64) (*contentFileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, list := range f.children {
		for _, item := range list {
			if item.ContentID == id {
				meta := item
				return &meta, nil
			}
		}
	}
	return nil, nil
}

func (f *fakeContentAPI) CreateFolder(title string, parentID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UnixMilli()
	f.children[parentID] = append(f.children[parentID], contentFileInfo{
		ContentID:   f.nextContentID(),
		ParentID:    parentID,
		Title:       title,
		Type:        contentTypeFolder,
		Status:      jsonNumber(contentStatusOK),
		CreateDate:  now,
		LastModDate: now,
	})
	return nil
}

// CreateFile 记录上传的文件记录（Location 指向 fake backend 的测试服务器）。
func (f *fakeContentAPI) CreateFile(rec uploadContentRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdFiles = append(f.createdFiles, rec)
	now := time.Now().UnixMilli()
	f.children[rec.ParentID] = append(f.children[rec.ParentID], contentFileInfo{
		ContentID:   f.nextContentID(),
		ParentID:    rec.ParentID,
		Title:       rec.Title,
		Type:        contentTypeFile,
		MimeType:    rec.MimeType,
		ContentSize: rec.ContentSize,
		Location:    rec.Location,
		Status:      jsonNumber(contentStatusOK),
		CreateDate:  now,
		LastModDate: now,
	})
	return nil
}

func (f *fakeContentAPI) UpdateFile(meta contentFileInfo, newTitle string, newParentID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	oldParent := meta.parentIDOf()
	// 从旧父目录移除
	oldList := f.children[oldParent]
	for i := range oldList {
		if oldList[i].ContentID == meta.ContentID {
			f.children[oldParent] = append(oldList[:i], oldList[i+1:]...)
			break
		}
	}
	// 放入新父目录（改名/换目录）
	meta.Title = newTitle
	meta.ParentID = newParentID
	meta.LastModDate = time.Now().UnixMilli()
	f.children[newParentID] = append(f.children[newParentID], meta)
	return nil
}

func (f *fakeContentAPI) DeleteContent(ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// ids 含被删项与其全部子孙；按 ContentID 过滤即可
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	for parent, list := range f.children {
		kept := list[:0]
		for _, item := range list {
			if !idSet[item.ContentID] {
				kept = append(kept, item)
			}
		}
		f.children[parent] = kept
	}
	return nil
}

// findRootFolder 返回 id=0 下指定 title 的文件夹（测试断言用）。
func (f *fakeContentAPI) findRootFolder(title string) (*contentFileInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.children[0] {
		if item.isFolder() && item.Title == title {
			meta := item
			return &meta, true
		}
	}
	return nil, false
}

// jsonNumber 把 status 数字转 json.RawMessage 形态（contentFileInfo.Status）。
func jsonNumber(n int) []byte {
	return []byte(fmt.Sprintf("%d", n))
}

// ---------------------------------------------------------------- fake uploadBackend

// fakeUploadBackend 存密文对象，并用 httptest.Server 提供下载 URL，
// 使 openRemoteContentFile 的 http.Get(meta.Location) 可以拉到密文。
type fakeUploadBackend struct {
	mu      sync.Mutex
	objects map[string][]byte // remotePath -> 密文
	server  *httptest.Server
}

func newFakeUploadBackend() *fakeUploadBackend {
	b := &fakeUploadBackend{objects: make(map[string][]byte)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		data, ok := b.objects[strings.TrimPrefix(r.URL.Path, "/")]
		b.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	})
	b.server = httptest.NewServer(mux)
	return b
}

// PutObject 存密文并返回指向本地测试服务器的 FileURL。
func (b *fakeUploadBackend) PutObject(ctx context.Context, remotePath string, reader io.Reader) (*obsUploadResult, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.objects[remotePath] = data
	b.mu.Unlock()
	return &obsUploadResult{
		FileURL: b.server.URL + "/" + remotePath,
		Key:     remotePath,
	}, nil
}

func (b *fakeUploadBackend) Close() {
	b.server.Close()
}

// ---------------------------------------------------------------- test fixture

// newTestFS 组装一套 fakes 并返回 fs 与带 Basic Auth 凭据的 ctx。
// 调用方负责 backend.Close()。
func newTestFS(t *testing.T, username, password string) (*contentFileSystem, context.Context, *fakeContentAPI, *fakeUploadBackend) {
	t.Helper()
	api := newFakeContentAPI()
	backend := newFakeUploadBackend()
	fs := &contentFileSystem{sessions: newSessionManager(api, backend)}
	ctx := withCreds(context.Background(), username, password)
	return fs, ctx, api, backend
}

// putFile 通过 fs 写一个文件并 Close（走完整加密上传 + CreateFile 流程）。
func putFile(t *testing.T, fs *contentFileSystem, ctx context.Context, name string, content []byte) {
	t.Helper()
	f, err := fs.OpenFile(ctx, name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("OpenFile(write) %s: %v", name, err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatalf("Write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close %s: %v", name, err)
	}
}

// getFile 通过 fs 读回整个文件（走 http.Get + 解密流程）。
func getFile(t *testing.T, fs *contentFileSystem, ctx context.Context, name string) []byte {
	t.Helper()
	f, err := fs.OpenFile(ctx, name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(read) %s: %v", name, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll %s: %v", name, err)
	}
	return data
}

// listDir 通过 fs 列出目录（返回 basename -> FileInfo）。
func listDir(t *testing.T, fs *contentFileSystem, ctx context.Context, name string) map[string]os.FileInfo {
	t.Helper()
	d, err := fs.OpenFile(ctx, name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(list) %s: %v", name, err)
	}
	defer d.Close()
	fis, err := d.Readdir(-1)
	if err != nil {
		t.Fatalf("Readdir %s: %v", name, err)
	}
	out := make(map[string]os.FileInfo, len(fis))
	for _, fi := range fis {
		out[fi.Name()] = fi
	}
	return out
}

// ---------------------------------------------------------------- root dir name

func TestRootDirNameStable(t *testing.T) {
	a1 := rootDirNameFor("alice", "wonder")
	a2 := rootDirNameFor("alice", "wonder")
	if a1 != a2 {
		t.Errorf("同一 username/password 应得到相同根目录名: %q != %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "udrive-") {
		t.Errorf("根目录名应以 udrive- 开头: %q", a1)
	}
	b := rootDirNameFor("alice", "different-pass")
	if a1 == b {
		t.Errorf("不同 password 应得到不同根目录名: %q == %q", a1, b)
	}
	c := rootDirNameFor("bob", "wonder")
	if a1 == c {
		t.Errorf("不同 username 应得到不同根目录名: %q == %q", a1, c)
	}
}

// ---------------------------------------------------------------- Mkdir / List / Delete

func TestMkdirListDelete(t *testing.T) {
	fs, ctx, api, backend := newTestFS(t, "alice", "pw1")
	defer backend.Close()

	// Mkdir "/dir"
	if err := fs.Mkdir(ctx, "/dir", 0755); err != nil {
		t.Fatalf("Mkdir /dir: %v", err)
	}

	// Stat 应是目录
	fi, err := fs.Stat(ctx, "/dir")
	if err != nil {
		t.Fatalf("Stat /dir: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("Stat /dir 应为目录, got mode=%v", fi.Mode())
	}
	if fi.Name() != "dir" {
		t.Errorf("Name() = %q, want %q", fi.Name(), "dir")
	}

	// 重复 Mkdir 应报 ErrExist
	if err := fs.Mkdir(ctx, "/dir", 0755); !os.IsExist(err) {
		t.Errorf("重复 Mkdir /dir 应返回 ErrExist, got %v", err)
	}

	// fake API 中应已创建用户根目录，dir 位于其下
	rootName := rootDirNameFor("alice", "pw1")
	root, ok := api.findRootFolder(rootName)
	if !ok {
		t.Fatalf("fake API 未创建用户根目录 %s", rootName)
	}
	kids, err := api.ListChildren(root.ContentID)
	if err != nil {
		t.Fatalf("ListChildren(root): %v", err)
	}
	dirListed := false
	for _, item := range kids {
		if item.Title == "dir" && item.isFolder() {
			dirListed = true
		}
	}
	if !dirListed {
		t.Errorf("dir 应位于用户根目录 %s 之下", rootName)
	}

	// RemoveAll "/dir"
	if err := fs.RemoveAll(ctx, "/dir"); err != nil {
		t.Fatalf("RemoveAll /dir: %v", err)
	}
	if _, err := fs.Stat(ctx, "/dir"); !os.IsNotExist(err) {
		t.Errorf("删除后 Stat /dir 应返回 ErrNotExist, got %v", err)
	}
}

// ---------------------------------------------------------------- PUT / GET encrypted round-trip

func TestPutGetEncrypted(t *testing.T) {
	fs, ctx, api, backend := newTestFS(t, "alice", "pw2")
	defer backend.Close()

	plain := []byte("hello udrive, this is a secret file for roundtrip testing")

	// PUT: OpenFile(O_CREATE|O_RDWR|O_TRUNC) + Write + Close
	putFile(t, fs, ctx, "/hello.txt", plain)

	// fake API 应收到 CreateFile 记录，且带非空 Location
	if len(api.createdFiles) != 1 {
		t.Fatalf("createdFiles 数量 = %d, want 1", len(api.createdFiles))
	}
	rec := api.createdFiles[0]
	if rec.Title != "hello.txt" {
		t.Errorf("记录 Title = %q, want %q", rec.Title, "hello.txt")
	}
	if rec.Type != contentTypeFile {
		t.Errorf("记录 Type = %d, want %d", rec.Type, contentTypeFile)
	}
	if rec.ContentSize != int64(len(plain)) {
		t.Errorf("记录 ContentSize = %d, want %d", rec.ContentSize, len(plain))
	}
	if rec.Location == "" {
		t.Fatal("记录 Location 为空")
	}
	if !strings.HasPrefix(rec.Location, backend.server.URL) {
		t.Errorf("记录 Location = %q, 应指向 fake backend 服务器", rec.Location)
	}

	// fake backend 对象不应包含明文（加密生效）
	if len(backend.objects) != 1 {
		t.Fatalf("fake backend 对象数 = %d, want 1", len(backend.objects))
	}
	for key, stored := range backend.objects {
		if bytes.Contains(stored, plain) {
			t.Errorf("对象 %s 的密文不应包含明文", key)
		}
		if bytes.Contains(stored, []byte("hello")) {
			t.Errorf("对象 %s 的密文不应包含任何明文片段", key)
		}
		if len(stored) <= len(plain) {
			t.Errorf("AES-GCM 分块加密应有 overhead: len(stored)=%d <= len(plain)=%d", len(stored), len(plain))
		}
	}

	// GET: OpenFile(O_RDONLY) + ReadAll 应还原明文
	got := getFile(t, fs, ctx, "/hello.txt")
	if !bytes.Equal(got, plain) {
		t.Errorf("GET 内容不匹配: got %q, want %q", got, plain)
	}

	// Stat /hello.txt 应有正确的 size
	fi, err := fs.Stat(ctx, "/hello.txt")
	if err != nil {
		t.Fatalf("Stat /hello.txt: %v", err)
	}
	if fi.Size() != int64(len(plain)) {
		t.Errorf("Stat size = %d, want %d", fi.Size(), len(plain))
	}
	if fi.IsDir() {
		t.Error("/hello.txt 不应是目录")
	}
}

// 大文件（多加密块）round-trip，验证分块加解密在 fs 层的正确性。
func TestPutGetLargeFile(t *testing.T) {
	fs, ctx, _, backend := newTestFS(t, "alice", "pw2")
	defer backend.Close()

	plain := make([]byte, blockSize*3+123)
	for i := range plain {
		plain[i] = byte(i % 251)
	}
	putFile(t, fs, ctx, "/big.bin", plain)

	got := getFile(t, fs, ctx, "/big.bin")
	if !bytes.Equal(got, plain) {
		t.Fatalf("大文件 roundtrip 不匹配: got %d bytes, want %d", len(got), len(plain))
	}

	fi, err := fs.Stat(ctx, "/big.bin")
	if err != nil {
		t.Fatalf("Stat /big.bin: %v", err)
	}
	if fi.Size() != int64(len(plain)) {
		t.Errorf("Stat size = %d, want %d", fi.Size(), len(plain))
	}
}

// ---------------------------------------------------------------- 目录内文件的列表

func TestListInsideDirectory(t *testing.T) {
	fs, ctx, _, backend := newTestFS(t, "alice", "pw2")
	defer backend.Close()

	if err := fs.Mkdir(ctx, "/docs", 0755); err != nil {
		t.Fatalf("Mkdir /docs: %v", err)
	}
	putFile(t, fs, ctx, "/docs/a.txt", []byte("aaa"))
	putFile(t, fs, ctx, "/docs/b.txt", []byte("bbb"))

	// Readdir /docs 应列出两个文件
	entries := listDir(t, fs, ctx, "/docs")
	if len(entries) != 2 {
		t.Fatalf("Readdir /docs 条目数 = %d, want 2", len(entries))
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		fi, ok := entries[name]
		if !ok {
			t.Errorf("Readdir /docs 缺少 %s", name)
			continue
		}
		if fi.IsDir() {
			t.Errorf("%s 不应是目录", name)
		}
		if fi.Size() != 3 {
			t.Errorf("%s size = %d, want 3", name, fi.Size())
		}
	}

	// 嵌套目录
	if err := fs.Mkdir(ctx, "/docs/sub", 0755); err != nil {
		t.Fatalf("Mkdir /docs/sub: %v", err)
	}
	putFile(t, fs, ctx, "/docs/sub/c.txt", []byte("ccc"))
	entries = listDir(t, fs, ctx, "/docs/sub")
	if len(entries) != 1 {
		t.Fatalf("Readdir /docs/sub 条目数 = %d, want 1", len(entries))
	}
	if _, ok := entries["c.txt"]; !ok {
		t.Errorf("Readdir /docs/sub 缺少 c.txt")
	}

	// 删除非空目录应连带子孙
	if err := fs.RemoveAll(ctx, "/docs"); err != nil {
		t.Fatalf("RemoveAll /docs: %v", err)
	}
	for _, p := range []string{"/docs", "/docs/a.txt", "/docs/sub", "/docs/sub/c.txt"} {
		if _, err := fs.Stat(ctx, p); !os.IsNotExist(err) {
			t.Errorf("删除后 Stat %s 应返回 ErrNotExist, got %v", p, err)
		}
	}
}

// ---------------------------------------------------------------- Rename

func TestRename(t *testing.T) {
	fs, ctx, _, backend := newTestFS(t, "alice", "pw3")
	defer backend.Close()

	plain := []byte("rename me")
	putFile(t, fs, ctx, "/a.txt", plain)

	// Rename /a.txt -> /b.txt
	if err := fs.Rename(ctx, "/a.txt", "/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// 旧路径 Stat 失败
	if _, err := fs.Stat(ctx, "/a.txt"); !os.IsNotExist(err) {
		t.Errorf("Rename 后 Stat /a.txt 应返回 ErrNotExist, got %v", err)
	}
	// 新路径 Stat 成功且内容可读
	fi, err := fs.Stat(ctx, "/b.txt")
	if err != nil {
		t.Fatalf("Stat /b.txt: %v", err)
	}
	if fi.Name() != "b.txt" {
		t.Errorf("Name() = %q, want %q", fi.Name(), "b.txt")
	}
	got := getFile(t, fs, ctx, "/b.txt")
	if !bytes.Equal(got, plain) {
		t.Errorf("Rename 后内容不匹配: got %q, want %q", got, plain)
	}

	// 目录改名 + 移动文件进目录
	if err := fs.Mkdir(ctx, "/olddir", 0755); err != nil {
		t.Fatalf("Mkdir /olddir: %v", err)
	}
	if err := fs.Rename(ctx, "/b.txt", "/olddir/moved.txt"); err != nil {
		t.Fatalf("Rename 进目录: %v", err)
	}
	if _, err := fs.Stat(ctx, "/b.txt"); !os.IsNotExist(err) {
		t.Errorf("移动后 Stat /b.txt 应返回 ErrNotExist, got %v", err)
	}
	got = getFile(t, fs, ctx, "/olddir/moved.txt")
	if !bytes.Equal(got, plain) {
		t.Errorf("移动后内容不匹配: got %q, want %q", got, plain)
	}
	if err := fs.Rename(ctx, "/olddir", "/newdir"); err != nil {
		t.Fatalf("目录 Rename: %v", err)
	}
	if _, err := fs.Stat(ctx, "/olddir/moved.txt"); !os.IsNotExist(err) {
		t.Errorf("目录改名后旧路径应 ErrNotExist, got %v", err)
	}
	got = getFile(t, fs, ctx, "/newdir/moved.txt")
	if !bytes.Equal(got, plain) {
		t.Errorf("目录改名后内容不匹配: got %q, want %q", got, plain)
	}
}

// ---------------------------------------------------------------- multi-user isolation

func TestMultiUserIsolation(t *testing.T) {
	// 两个用户共用同一个 manager（即同一后端账号）—— 这是产品行为：
	// 多用户共用一个 getToken() 后端，隔离完全靠凭据派生的根目录与密钥。
	api := newFakeContentAPI()
	backend := newFakeUploadBackend()
	defer backend.Close()
	fs := &contentFileSystem{sessions: newSessionManager(api, backend)}
	ctxA := withCreds(context.Background(), "alice", "pw-a")
	ctxB := withCreds(context.Background(), "bob", "pw-b")

	// 用户 A 写入文件
	plainA := []byte("alice private data")
	putFile(t, fs, ctxA, "/secret.txt", plainA)

	// 用户 B 看不到 A 的文件
	if _, err := fs.Stat(ctxB, "/secret.txt"); !os.IsNotExist(err) {
		t.Errorf("user B Stat /secret.txt 应返回 ErrNotExist, got %v", err)
	}

	// 用户 B 写同名文件，互不覆盖
	plainB := []byte("bob data")
	putFile(t, fs, ctxB, "/secret.txt", plainB)

	// 两个用户在 fake API 中有不同的根目录
	rootAName := rootDirNameFor("alice", "pw-a")
	rootBName := rootDirNameFor("bob", "pw-b")
	_, okA := api.findRootFolder(rootAName)
	_, okB := api.findRootFolder(rootBName)
	if !okA || !okB {
		t.Fatalf("两个用户的根目录都应被创建: A=%v B=%v", okA, okB)
	}
	if rootAName == rootBName {
		t.Error("两个用户应派生不同的根目录名")
	}

	// 各自读回自己的文件，内容正确
	if got := getFile(t, fs, ctxA, "/secret.txt"); !bytes.Equal(got, plainA) {
		t.Errorf("user A 读回内容不匹配: got %q, want %q", got, plainA)
	}
	if got := getFile(t, fs, ctxB, "/secret.txt"); !bytes.Equal(got, plainB) {
		t.Errorf("user B 读回内容不匹配: got %q, want %q", got, plainB)
	}

	// 同一用户改密码 -> 新根目录，旧文件不可见（新空间）
	ctxA2 := withCreds(context.Background(), "alice", "pw-a2")
	if _, err := fs.Stat(ctxA2, "/secret.txt"); !os.IsNotExist(err) {
		t.Errorf("改密码后旧文件应不可见, got %v", err)
	}

	// B 删除自己的文件不影响 A
	if err := fs.RemoveAll(ctxB, "/secret.txt"); err != nil {
		t.Fatalf("user B RemoveAll: %v", err)
	}
	if got := getFile(t, fs, ctxA, "/secret.txt"); !bytes.Equal(got, plainA) {
		t.Errorf("B 删除自己的文件不应影响 A: got %q, want %q", got, plainA)
	}
}

// ---------------------------------------------------------------- 缓存失效

// TestSessionCacheInvalidation 验证 fs 写操作后能立即看到变化（15s 缓存被 invalidate）。
func TestSessionCacheInvalidation(t *testing.T) {
	fs, ctx, _, backend := newTestFS(t, "alice", "pw-cache")
	defer backend.Close()

	// 先列一次根目录（填充缓存）
	listDir(t, fs, ctx, "/")

	// Mkdir 后（应 invalidate 缓存）立刻 Stat 应能找到
	if err := fs.Mkdir(ctx, "/fresh-dir", 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := fs.Stat(ctx, "/fresh-dir"); err != nil {
		t.Errorf("Mkdir 后立刻 Stat 应成功（缓存被 invalidate）: %v", err)
	}

	// 写文件后立刻 Stat / 读回
	putFile(t, fs, ctx, "/fresh.txt", []byte("fresh"))
	if _, err := fs.Stat(ctx, "/fresh.txt"); err != nil {
		t.Errorf("PUT 后立刻 Stat 应成功: %v", err)
	}

	// 删除后立刻 Stat 应消失
	if err := fs.RemoveAll(ctx, "/fresh-dir"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := fs.Stat(ctx, "/fresh-dir"); !os.IsNotExist(err) {
		t.Errorf("RemoveAll 后立刻 Stat 应 ErrNotExist, got %v", err)
	}
}
