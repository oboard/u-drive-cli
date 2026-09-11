package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// content_fs.go — 基于 uLearning 内容 API 的 WebDAV 文件系统。
//
// 目录/文件列表以 uLearning content API 为真相来源，不再使用本地索引。
// 文件字节仍通过 upload-only OBS 上传，写入时加密，读取时按记录 location 拉取并解密。
// 多用户隔离：Basic Auth 的 username/password 派生根目录名与加密密钥。

type uploadBackend interface {
	PutObject(ctx context.Context, remotePath string, reader io.Reader) (*obsUploadResult, error)
}

type obsUploadBackend struct{}

func (obsUploadBackend) PutObject(ctx context.Context, remotePath string, reader io.Reader) (*obsUploadResult, error) {
	return uploadReaderToObs(reader, remotePath, false)
}

type sessionManager struct {
	mu       sync.Mutex
	api      apiClient
	backend  uploadBackend
	sessions map[string]*contentSession
}

func newSessionManager(api apiClient, backend uploadBackend) *sessionManager {
	return &sessionManager{api: api, backend: backend, sessions: make(map[string]*contentSession)}
}

func sessionKeyFor(username, password string) string {
	h := sha256.New()
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(password))
	return hex.EncodeToString(h.Sum(nil))
}

func rootDirNameFor(username, password string) string {
	h := sha256.New()
	h.Write([]byte("udrive-root-v1\x00"))
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(password))
	return "udrive-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func (m *sessionManager) get(username, password string) (*contentSession, error) {
	key := sessionKeyFor(username, password)
	m.mu.Lock()
	defer m.mu.Unlock()
	if sess, ok := m.sessions[key]; ok {
		return sess, nil
	}
	sess := &contentSession{
		api:      m.api,
		backend:  m.backend,
		rootName: rootDirNameFor(username, password),
		key:      deriveKey(username, password),
		cache:    make(map[int64]*childrenCache),
	}
	m.sessions[key] = sess
	return sess, nil
}

type childrenCache struct {
	items   map[string]*contentFileInfo
	expires time.Time
}

type contentSession struct {
	mu       sync.Mutex
	api      apiClient
	backend  uploadBackend
	rootName string
	rootID   int64
	key      []byte
	cache    map[int64]*childrenCache
}

func (s *contentSession) root(ctx context.Context) (int64, error) {
	s.mu.Lock()
	if s.rootID != 0 {
		id := s.rootID
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()

	children, err := s.api.ListChildren(0)
	if err != nil {
		return 0, err
	}
	for i := range children {
		if children[i].isFolder() && children[i].Title == s.rootName {
			s.mu.Lock()
			s.rootID = children[i].ContentID
			s.mu.Unlock()
			return children[i].ContentID, nil
		}
	}
	if err := s.api.CreateFolder(s.rootName, 0); err != nil {
		return 0, err
	}
	children, err = s.api.ListChildren(0)
	if err != nil {
		return 0, err
	}
	for i := range children {
		if children[i].isFolder() && children[i].Title == s.rootName {
			s.mu.Lock()
			s.rootID = children[i].ContentID
			s.mu.Unlock()
			return children[i].ContentID, nil
		}
	}
	return 0, fmt.Errorf("创建用户根目录后仍找不到: %s", s.rootName)
}

func (s *contentSession) listChildren(parentID int64) ([]contentFileInfo, error) {
	s.mu.Lock()
	cached := s.cache[parentID]
	if cached != nil && time.Now().Before(cached.expires) {
		list := make([]contentFileInfo, 0, len(cached.items))
		for _, item := range cached.items {
			list = append(list, *item)
		}
		s.mu.Unlock()
		return list, nil
	}
	s.mu.Unlock()

	list, err := s.api.ListChildren(parentID)
	if err != nil {
		return nil, err
	}
	items := make(map[string]*contentFileInfo)
	for i := range list {
		item := list[i]
		if prev, ok := items[item.Title]; !ok || item.CreateDate > prev.CreateDate {
			items[item.Title] = &item
		}
	}
	s.mu.Lock()
	s.cache[parentID] = &childrenCache{items: items, expires: time.Now().Add(15 * time.Second)}
	s.mu.Unlock()
	return list, nil
}

func (s *contentSession) invalidate(parentID int64) {
	s.mu.Lock()
	delete(s.cache, parentID)
	s.mu.Unlock()
}

func (s *contentSession) resolveChild(parentID int64, title string) (*contentFileInfo, error) {
	list, err := s.listChildren(parentID)
	if err != nil {
		return nil, err
	}
	var best *contentFileInfo
	for i := range list {
		if list[i].Title == title && (best == nil || list[i].CreateDate > best.CreateDate) {
			item := list[i]
			best = &item
		}
	}
	if best == nil {
		return nil, os.ErrNotExist
	}
	return best, nil
}

func (s *contentSession) resolve(ctx context.Context, name string) (*contentFileInfo, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, err
	}
	if cleaned == "/" {
		rootID, err := s.root(ctx)
		if err != nil {
			return nil, err
		}
		// WebDAV 的 / 对应内容 API 中该用户的 hash 根目录，而不是 account 根(parentId=0)。
		return &contentFileInfo{ContentID: rootID, Title: "/", Type: contentTypeFolder, ContentSize: 0}, nil
	}
	parentID, err := s.root(ctx)
	if err != nil {
		return nil, err
	}
	segments := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	var item *contentFileInfo
	for i, seg := range segments {
		item, err = s.resolveChild(parentID, seg)
		if err != nil {
			return nil, err
		}
		if i < len(segments)-1 {
			if !item.isFolder() {
				return nil, os.ErrNotExist
			}
			parentID = item.ContentID
		}
	}
	return item, nil
}

func (s *contentSession) resolveParent(ctx context.Context, name string) (int64, string, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return 0, "", err
	}
	if cleaned == "/" {
		return 0, "", fmt.Errorf("根目录没有父目录")
	}
	base := path.Base(cleaned)
	parentPath := parentWebDAVPath(cleaned)
	if parentPath == "/" {
		id, err := s.root(ctx)
		return id, base, err
	}
	parent, err := s.resolve(ctx, parentPath)
	if err != nil {
		return 0, "", err
	}
	if !parent.isFolder() {
		return 0, "", os.ErrNotExist
	}
	return parent.ContentID, base, nil
}

type contentFileSystem struct{ sessions *sessionManager }

func newContentFileSystem() *contentFileSystem {
	return &contentFileSystem{sessions: newSessionManager(newHTTPContentAPI(getToken), obsUploadBackend{})}
}

func (fs *contentFileSystem) session(ctx context.Context) (*contentSession, error) {
	u, p, ok := credsFromCtx(ctx)
	if !ok {
		return nil, os.ErrPermission
	}
	return fs.sessions.get(u, p)
}

func (fs *contentFileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	s, err := fs.session(ctx)
	if err != nil {
		return nil, err
	}
	info, err := s.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	return contentFileInfoToOS(name, info), nil
}

func (fs *contentFileSystem) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	s, err := fs.session(ctx)
	if err != nil {
		return err
	}
	parentID, title, err := s.resolveParent(ctx, name)
	if err != nil {
		return err
	}
	if _, err := s.resolveChild(parentID, title); err == nil {
		return os.ErrExist
	}
	if err := s.api.CreateFolder(title, parentID); err != nil {
		return err
	}
	s.invalidate(parentID)
	return nil
}

func (fs *contentFileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	s, err := fs.session(ctx)
	if err != nil {
		return nil, err
	}
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, err
	}
	isWrite := flag&os.O_CREATE != 0 || flag&os.O_TRUNC != 0 || flag&os.O_RDWR != 0 || flag&os.O_WRONLY != 0
	if isWrite {
		parentID, title, err := s.resolveParent(ctx, cleaned)
		if err != nil {
			return nil, err
		}
		// 若同名文件已存在（覆盖场景），捕获其 meta，
		// Close 时走 UpdateFile 让后端覆盖原记录，避免被自动重命名为 "(1).txt"。
		existing, _ := s.resolveChild(parentID, title)
		var existingMeta *contentFileInfo
		if existing != nil && !existing.isFolder() {
			existingMeta = existing
		}
		return &contentFile{
			session:  s,
			name:     cleaned,
			parentID: parentID,
			title:    title,
			meta:     existingMeta,
			write:    true,
			buf:      &bytes.Buffer{}, // 内存缓冲，Close 时流式加密上传
		}, nil
	}
	meta, err := s.resolve(ctx, cleaned)
	if err != nil {
		return nil, err
	}
	if meta.isFolder() {
		return &contentDir{session: s, name: cleaned, meta: meta}, nil
	}
	// 读文件：延迟加载，只在 Read/Seek 时才下载
	return &contentFile{session: s, name: cleaned, meta: meta}, nil
}

func (fs *contentFileSystem) RemoveAll(ctx context.Context, name string) error {
	s, err := fs.session(ctx)
	if err != nil {
		return err
	}
	meta, err := s.resolve(ctx, name)
	if err != nil {
		return err
	}
	ids, err := s.collectIDs(meta)
	if err != nil {
		return err
	}
	if err := s.api.DeleteContent(ids); err != nil {
		return err
	}
	parentID, _, _ := s.resolveParent(ctx, name)
	s.invalidate(parentID)
	return nil
}

func (fs *contentFileSystem) Rename(ctx context.Context, oldName, newName string) error {
	s, err := fs.session(ctx)
	if err != nil {
		return err
	}
	oldMeta, err := s.resolve(ctx, oldName)
	if err != nil {
		return err
	}
	newParentID, newTitle, err := s.resolveParent(ctx, newName)
	if err != nil {
		return err
	}
	// Rename 不改文件内容，传空 location/size/mime 让 UpdateFile 保留原值。
	if err := s.api.UpdateFile(*oldMeta, newTitle, newParentID, "", 0, ""); err != nil {
		return err
	}
	oldParentID := oldMeta.parentIDOf()
	s.invalidate(oldParentID)
	s.invalidate(newParentID)
	return nil
}

func (s *contentSession) collectIDs(meta *contentFileInfo) ([]int64, error) {
	ids := []int64{meta.ContentID}
	if meta.isFolder() {
		children, err := s.listChildren(meta.ContentID)
		if err != nil {
			return nil, err
		}
		for i := range children {
			childIDs, err := s.collectIDs(&children[i])
			if err != nil {
				return nil, err
			}
			ids = append(ids, childIDs...)
		}
	}
	return ids, nil
}

func contentFileInfoToOS(name string, meta *contentFileInfo) os.FileInfo {
	filename := path.Base(name)
	if name == "/" {
		filename = "/"
	}
	return &contentInfo{name: filename, size: meta.ContentSize, modTime: meta.modTime(), isDir: meta.isFolder()}
}

type contentInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (i *contentInfo) Name() string { return i.name }
func (i *contentInfo) Size() int64  { return i.size }
func (i *contentInfo) Mode() os.FileMode {
	if i.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (i *contentInfo) ModTime() time.Time { return i.modTime }
func (i *contentInfo) IsDir() bool        { return i.isDir }
func (i *contentInfo) Sys() any           { return nil }

type contentDir struct {
	session *contentSession
	name    string
	meta    *contentFileInfo
	infos   []os.FileInfo
	offset  int
}

func (d *contentDir) Read([]byte) (int, error)       { return 0, fmt.Errorf("目录不支持读取") }
func (d *contentDir) Seek(int64, int) (int64, error) { return 0, fmt.Errorf("目录不支持 Seek") }
func (d *contentDir) Write([]byte) (int, error)      { return 0, fmt.Errorf("目录不支持写入") }
func (d *contentDir) Close() error                   { return nil }
func (d *contentDir) Stat() (os.FileInfo, error)     { return contentFileInfoToOS(d.name, d.meta), nil }
func (d *contentDir) Readdir(count int) ([]os.FileInfo, error) {
	if d.infos == nil {
		items, err := d.session.listChildren(d.meta.ContentID)
		if err != nil {
			return nil, err
		}
		d.infos = make([]os.FileInfo, 0, len(items))
		for i := range items {
			d.infos = append(d.infos, contentFileInfoToOS(items[i].Title, &items[i]))
		}
	}
	if count <= 0 {
		return d.infos, nil
	}
	if d.offset >= len(d.infos) {
		return nil, io.EOF
	}
	end := d.offset + count
	if end > len(d.infos) {
		end = len(d.infos)
	}
	out := d.infos[d.offset:end]
	d.offset = end
	return out, nil
}

type contentFile struct {
	session  *contentSession
	name     string
	parentID int64
	title    string
	meta     *contentFileInfo
	write    bool          // 写场景标记
	buf      *bytes.Buffer // 写场景：内存缓冲
	// 读场景：
	data    []byte // 已解密数据缓存
	readPos int    // 当前读位置
}

const maxReadCacheSize = 4 * 1024 * 1024 // 4MB 读缓存上限

// fetchRange 获取 [start, end) 范围的解密数据。
// 如果 start 在缓存内，复用缓存；否则重新下载。
func (f *contentFile) fetchRange(start int) error {
	// start 在缓存范围内，无需重新下载
	if start <= len(f.data) && len(f.data) > 0 {
		return nil
	}

	// 需要重新下载，丢弃旧缓存
	f.data = f.data[:0]
	f.readPos = 0

	resp, err := http.Get(f.meta.Location)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return fmt.Errorf("下载远端文件失败: %s", resp.Status)
	}
	dec, err := newDecryptReader(resp.Body, f.session.key)
	if err != nil {
		resp.Body.Close()
		return err
	}

	// 读取到 start 位置（丢弃）
	if start > 0 {
		discard := make([]byte, 4096)
		need := start
		for need > 0 {
			toRead := len(discard)
			if toRead > need {
				toRead = need
			}
			n, err := dec.Read(discard[:toRead])
			need -= n
			if err != nil {
				if err == io.EOF {
					break
				}
				resp.Body.Close()
				return err
			}
		}
	}

	// 从 start 开始读取并缓存（最多 maxReadCacheSize）
	cacheCap := maxReadCacheSize
	if f.meta.ContentSize-int64(start) < int64(maxReadCacheSize) {
		cacheCap = int(f.meta.ContentSize - int64(start) + 1)
	}
	f.data = make([]byte, 0, cacheCap)
	buf := make([]byte, 4096)
	for {
		toRead := len(buf)
		if len(f.data)+toRead > cap(f.data) {
			toRead = cap(f.data) - len(f.data)
			if toRead <= 0 {
				break
			}
		}
		n, err := dec.Read(buf[:toRead])
		if n > 0 {
			f.data = append(f.data, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (f *contentFile) Read(p []byte) (int, error) {
	if f.write {
		return 0, fmt.Errorf("写入模式不支持读")
	}
	// 确保当前位置的数据已缓存
	if err := f.fetchRange(f.readPos); err != nil {
		return 0, err
	}
	// 从缓存读取
	if f.readPos >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.readPos:])
	f.readPos += n
	return n, nil
}

func (f *contentFile) Seek(offset int64, whence int) (int64, error) {
	if f.write {
		return 0, fmt.Errorf("写入模式不支持 Seek")
	}
	// 计算目标位置
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = int64(f.readPos) + offset
	case io.SeekEnd:
		target = f.meta.ContentSize + offset
	default:
		return 0, fmt.Errorf("无效的 whence: %d", whence)
	}
	if target < 0 {
		return 0, fmt.Errorf("负位置")
	}
	f.readPos = int(target)
	return target, nil
}
func (f *contentFile) Write(p []byte) (int, error) {
	if f.buf != nil {
		return f.buf.Write(p)
	}
	return 0, fmt.Errorf("写入未初始化")
}
func (f *contentFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("%s 不是目录", f.name)
}
func (f *contentFile) Stat() (os.FileInfo, error) {
	if f.write {
		size := int64(0)
		if f.buf != nil {
			size = int64(f.buf.Len())
		}
		return &contentInfo{name: f.title, size: size, modTime: time.Now(), isDir: false}, nil
	}
	return contentFileInfoToOS(f.name, f.meta), nil
}

func (f *contentFile) Close() error {
	// 读场景：释放缓存
	if !f.write {
		f.data = nil
		return nil
	}
	// 写场景：从内存缓冲流式加密上传
	if f.buf == nil {
		return nil
	}
	plainSize := int64(f.buf.Len())
	enc, err := newEncryptReader(f.buf, f.session.key)
	if err != nil {
		return err
	}
	flat := flatOBSKey(f.session.rootName, time.Now().UnixMilli())
	result, err := f.session.backend.PutObject(context.Background(), flat, enc)
	if err != nil {
		return err
	}
	ext := extNoDot(f.title)
	if f.meta != nil {
		// 覆盖：保留原 contentId，更新 location/size/mime，后端不会自动 "(1)" 重命名。
		if err := f.session.api.UpdateFile(*f.meta, f.title, f.parentID, result.FileURL, plainSize, ext); err != nil {
			return err
		}
	} else {
		if err := f.session.api.CreateFile(uploadContentRecord{
			Title:       f.title,
			Type:        contentTypeFile,
			Status:      contentStatusOK,
			ContentSize: plainSize,
			Location:    result.FileURL,
			MimeType:    ext,
			IsView:      0,
			Remark2:     1,
			Remark3:     0,
			ParentID:    f.parentID,
		}); err != nil {
			return err
		}
	}
	f.session.invalidate(f.parentID)
	return nil
}

func extNoDot(name string) string {
	ext := path.Ext(name)
	if strings.HasPrefix(ext, ".") {
		return strings.TrimPrefix(ext, ".")
	}
	return ext
}
