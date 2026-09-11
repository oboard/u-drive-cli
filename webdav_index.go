package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type uploadBackend interface {
	PutObject(ctx context.Context, remotePath string, reader io.Reader) (*obsUploadResult, error)
	ClearObject(ctx context.Context, remotePath string) error
}

// --- 多用户运行时鉴权（凭证即密钥） ---
//
// 启动时无需预配任何账号。每次 WebDAV 请求经 Basic Auth 的 username+password，
// auth 中间件把凭据注入 request context；FileSystem 从 ctx 取凭据，找到/创建
// 对应用户的索引（密钥 = KDF(username, password)，索引/缓存按 username 分目录）。

type credsCtxKey struct{}

// withCreds 把鉴权后的 username/password 写入上下文。
func withCreds(ctx context.Context, username, password string) context.Context {
	return context.WithValue(ctx, credsCtxKey{}, [2]string{username, password})
}

// credsFromCtx 从上下文读取 username/password。
func credsFromCtx(ctx context.Context) (string, string, bool) {
	v, ok := ctx.Value(credsCtxKey{}).([2]string)
	if !ok {
		return "", "", false
	}
	return v[0], v[1], true
}

// userIndexes 按 username 惰性构建各自的 webDAVIndex（密钥、索引、缓存均按用户隔离）。
type userIndexes struct {
	mu      sync.Mutex
	baseDir string
	backend uploadBackend
	cache   map[string]*webDAVIndex
}

func newUserIndexes(baseDir string, backend uploadBackend) *userIndexes {
	return &userIndexes{
		baseDir: baseDir,
		backend: backend,
		cache:   make(map[string]*webDAVIndex),
	}
}

// indexFor 返回 username 对应的索引；首访该用户时才用其密码派生密钥并初始化目录。
func (s *userIndexes) indexFor(username, password string) (*webDAVIndex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idx, ok := s.cache[username]; ok {
		return idx, nil
	}

	userBase := sanitizeUserDir(username)
	indexPath := filepath.Join(s.baseDir, userBase, "webdav-index.json")
	cacheDir := filepath.Join(s.baseDir, userBase, "webdav-cache")

	dek := deriveKey(username, password)
	idx, err := newWebDAVIndex(indexPath, cacheDir, s.backend, dek, username)
	if err != nil {
		return nil, err
	}
	s.cache[username] = idx
	return idx, nil
}

// sanitizeUserDir 清洗 username 用作目录名，防止路径穿越。
func sanitizeUserDir(username string) string {
	u := strings.ReplaceAll(username, "\\", "/")
	u = strings.Trim(u, "/")
	if u == "" || u == "." || u == ".." {
		return "_default"
	}
	for _, part := range strings.Split(u, "/") {
		if part == ".." {
			return "_default"
		}
	}
	return u
}

type obsUploadBackend struct{}

func (obsUploadBackend) PutObject(ctx context.Context, remotePath string, reader io.Reader) (*obsUploadResult, error) {
	// remotePath 已是扁平 key（无 "/"，含 username），直接用。
	return uploadReaderToObs(reader, remotePath, false)
}

func (obsUploadBackend) ClearObject(ctx context.Context, remotePath string) error {
	return clearRemoteObject(remotePath)
}

type webDAVIndex struct {
	mu        sync.Mutex
	indexPath string
	cacheDir  string
	backend   uploadBackend
	dek       []byte                       // 数据密钥：内容/索引用它加密
	username  string                       // 用户 ID：用于扁平 OBS key 前缀
	Entries   map[string]*webDAVIndexEntry `json:"entries"`
}

type webDAVIndexEntry struct {
	Path       string    `json:"path"`
	RemotePath string    `json:"remotePath,omitempty"`
	Key        string    `json:"key,omitempty"`
	IsDir      bool      `json:"isDir"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"modTime"`
	FileURL    string    `json:"fileUrl,omitempty"`
	SourceURL  string    `json:"sourceUrl,omitempty"`
	CacheName  string    `json:"cacheName,omitempty"`
}

func newWebDAVIndex(indexPath, cacheDir string, backend uploadBackend, dek []byte, username string) (*webDAVIndex, error) {
	if indexPath == "" || cacheDir == "" {
		defaultIndexPath, defaultCacheDir, err := defaultWebDAVPaths()
		if err != nil {
			return nil, err
		}
		if indexPath == "" {
			indexPath = defaultIndexPath
		}
		if cacheDir == "" {
			cacheDir = defaultCacheDir
		}
	}

	idx := &webDAVIndex{
		indexPath: indexPath,
		cacheDir:  cacheDir,
		backend:   backend,
		dek:       dek,
		username:  username,
		Entries:   make(map[string]*webDAVIndexEntry),
	}

	if err := os.MkdirAll(filepath.Dir(indexPath), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(indexPath)
	if err == nil {
		// 索引加密了：用 DEK 解密。
		if len(data) >= gcmNonceSize+gcmTagSize {
			if dek != nil {
				plain, openErr := aeadOpen(dek, data)
				if openErr != nil {
					return nil, fmt.Errorf("解密 WebDAV 索引失败（密码或密钥错误）: %v", openErr)
				}
				data = plain
			} else {
				// 无 DEK：尝试直接把整段当 JSON 解析（兼容未加密的旧索引）。
			}
		}
		if err := json.Unmarshal(data, idx); err != nil {
			return nil, fmt.Errorf("读取 WebDAV 索引失败: %v", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if idx.Entries == nil {
		idx.Entries = make(map[string]*webDAVIndexEntry)
	}
	if _, ok := idx.Entries["/"]; !ok {
		idx.Entries["/"] = &webDAVIndexEntry{Path: "/", IsDir: true, ModTime: time.Now()}
		if err := idx.saveLocked(); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

func defaultDataDir() (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil || cacheRoot == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", fmt.Errorf("获取缓存目录失败: %v", err)
		}
		cacheRoot = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheRoot, "udrive"), nil
}

func defaultWebDAVPaths() (string, string, error) {
	base, err := defaultDataDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(base, "webdav-index.json"), filepath.Join(base, "webdav-cache"), nil
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

func remotePathFromDAVPath(name string) (string, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return "", err
	}
	if cleaned == "/" {
		return "", fmt.Errorf("根目录没有远程对象路径")
	}
	return strings.TrimPrefix(cleaned, "/"), nil
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

func cacheNameForPath(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func (idx *webDAVIndex) stat(name string) (os.FileInfo, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	entry, ok := idx.Entries[cleaned]
	if !ok {
		return nil, os.ErrNotExist
	}
	return entry.fileInfo(), nil
}

func (idx *webDAVIndex) mkdir(name string) error {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return err
	}
	if cleaned == "/" {
		return os.ErrExist
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	if _, ok := idx.Entries[cleaned]; ok {
		return os.ErrExist
	}
	parent := idx.Entries[parentWebDAVPath(cleaned)]
	if parent == nil || !parent.IsDir {
		return os.ErrNotExist
	}

	idx.Entries[cleaned] = &webDAVIndexEntry{Path: cleaned, IsDir: true, ModTime: time.Now()}
	return idx.saveLocked()
}

func (idx *webDAVIndex) listChildren(name string) ([]os.FileInfo, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	entry, ok := idx.Entries[cleaned]
	if !ok {
		return nil, os.ErrNotExist
	}
	if !entry.IsDir {
		return nil, fmt.Errorf("%s 不是目录", cleaned)
	}

	children := make([]os.FileInfo, 0)
	for childPath, child := range idx.Entries {
		if childPath == cleaned {
			continue
		}
		if parentWebDAVPath(childPath) == cleaned {
			children = append(children, child.fileInfo())
		}
	}

	sort.Slice(children, func(i, j int) bool {
		return children[i].Name() < children[j].Name()
	})
	return children, nil
}

func (idx *webDAVIndex) openCacheFile(name string) (*os.File, string, os.FileInfo, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, "", nil, err
	}

	entry, err := idx.entry(cleaned)
	if err != nil {
		return nil, "", nil, err
	}
	if entry.IsDir {
		return nil, "", nil, fmt.Errorf("%s 是目录", cleaned)
	}

	cachePath, err := idx.ensureCache(entry)
	if err != nil {
		return nil, "", nil, err
	}

	if idx.dek == nil {
		file, err := os.Open(cachePath)
		if err != nil {
			return nil, "", nil, err
		}
		return file, "", entry.fileInfo(), nil
	}

	// 解密缓存密文 -> 明文临时文件（供 ServeContent / Range / Seek）。
	decryptedTemp, err := idx.decryptCacheToPlain(cachePath)
	if err != nil {
		return nil, "", nil, err
	}
	return decryptedTemp.file, decryptedTemp.path, entry.fileInfo(), nil
}

// plainDecryptedFile 是解密缓存得到的临时明文文件及其路径。
type plainDecryptedFile struct {
	file *os.File
	path string
}

// decryptCacheToPlain 将密文缓存解密到缓存目录下的明文临时文件。
func (idx *webDAVIndex) decryptCacheToPlain(cachePath string) (*plainDecryptedFile, error) {
	src, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	dec, err := newDecryptReader(src, idx.dek)
	if err != nil {
		src.Close()
		return nil, err
	}
	if err := os.MkdirAll(idx.cacheDir, 0755); err != nil {
		src.Close()
		return nil, err
	}
	tmp, err := os.CreateTemp(idx.cacheDir, "webdav-plain-*")
	if err != nil {
		src.Close()
		return nil, err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, dec); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		src.Close()
		return nil, err
	}
	src.Close()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	return &plainDecryptedFile{file: tmp, path: tmpName}, nil
}

func (idx *webDAVIndex) prepareWrite(name string, flag int) (*os.File, string, string, error) {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return nil, "", "", err
	}
	if cleaned == "/" {
		return nil, "", "", fmt.Errorf("不能写入根目录")
	}

	idx.mu.Lock()
	entry, exists := idx.Entries[cleaned]
	parent := idx.Entries[parentWebDAVPath(cleaned)]
	idx.mu.Unlock()

	if parent == nil || !parent.IsDir {
		return nil, "", "", os.ErrNotExist
	}
	if exists && entry.IsDir {
		return nil, "", "", fmt.Errorf("%s 是目录", cleaned)
	}
	if !exists && flag&os.O_CREATE == 0 {
		return nil, "", "", os.ErrNotExist
	}
	if exists && flag&os.O_EXCL != 0 {
		return nil, "", "", os.ErrExist
	}

	if err := os.MkdirAll(idx.cacheDir, 0755); err != nil {
		return nil, "", "", err
	}
	tempFile, err := os.CreateTemp(idx.cacheDir, "webdav-upload-*")
	if err != nil {
		return nil, "", "", err
	}

	if exists && flag&os.O_TRUNC == 0 {
		cachePath, cacheErr := idx.ensureCache(entry.clone())
		if cacheErr != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, "", "", cacheErr
		}

		var copySource io.Reader
		if idx.dek != nil {
			// 缓存是密文：解密后用明文续写。
			decSrc, decErr := os.Open(cachePath)
			if decErr != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				return nil, "", "", decErr
			}
			dec, decErr := newDecryptReader(decSrc, idx.dek)
			if decErr != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				decSrc.Close()
				return nil, "", "", decErr
			}
			copySource = dec
		} else {
			f, fErr := os.Open(cachePath)
			if fErr != nil {
				tempFile.Close()
				os.Remove(tempFile.Name())
				return nil, "", "", fErr
			}
			copySource = f
		}
		_, copyErr := io.Copy(tempFile, copySource)
		if c, ok := copySource.(io.Closer); ok {
			_ = c.Close()
		}
		if copyErr != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, "", "", copyErr
		}
	}

	if flag&os.O_APPEND != 0 {
		if _, err := tempFile.Seek(0, io.SeekEnd); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, "", "", err
		}
	} else {
		if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, "", "", err
		}
	}

	return tempFile, tempFile.Name(), cleaned, nil
}

func (idx *webDAVIndex) commitUpload(ctx context.Context, name, tempPath string, file *os.File) error {
	realPath, err := remotePathFromDAVPath(name)
	if err != nil {
		return err
	}
	// 后端禁止含 "/" 的 key：将真实路径摊平成单段扁平 key（含 username + 时间戳）。
	remotePath := flatRemoteKey(idx.username, realPath)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	// 先取明文 size（WebDAV 元数据/Content-Length 用明文长度）。
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()

	// 无 DEK：原样上传明文 temp，缓存就是明文（兼容旧行为）。
	if idx.dek == nil {
		return idx.uploadPlain(ctx, name, remotePath, tempPath, file, size)
	}

	// 有 DEK：加密 temp -> 密文 temp，上传密文，并作为缓存落盘。
	cacheName := cacheNameForPath(name)
	cachePath := filepath.Join(idx.cacheDir, cacheName)
	cipherTemp, err := os.CreateTemp(idx.cacheDir, "webdav-cipher-*")
	if err != nil {
		return err
	}
	cipherTempName := cipherTemp.Name()

	enc, encErr := newEncryptReader(file, idx.dek)
	if encErr != nil {
		cipherTemp.Close()
		os.Remove(cipherTempName)
		return encErr
	}
	if _, copyErr := io.Copy(cipherTemp, enc); copyErr != nil {
		cipherTemp.Close()
		os.Remove(cipherTempName)
		return copyErr
	}
	if err := cipherTemp.Close(); err != nil {
		os.Remove(cipherTempName)
		return err
	}

	// 用独立句柄喂给 SDK，远端上传密文。
	uploadHandle, err := os.Open(cipherTempName)
	if err != nil {
		os.Remove(cipherTempName)
		return err
	}
	result, err := idx.backend.PutObject(ctx, remotePath, uploadHandle)
	_ = uploadHandle.Close()
	if err != nil {
		os.Remove(cipherTempName)
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(cipherTempName)
		return err
	}

	// 密文 temp 落到缓存。
	if err := replaceFile(cipherTempName, cachePath); err != nil {
		return err
	}

	// 更新索引。
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.Entries[name] = &webDAVIndexEntry{
		Path:       name,
		RemotePath: remotePath,
		Key:        result.Key,
		IsDir:      false,
		Size:       size,
		ModTime:    time.Now(),
		FileURL:    result.FileURL,
		SourceURL:  result.SourceURL,
		CacheName:  cacheName,
	}
	if parent := idx.Entries[parentWebDAVPath(name)]; parent != nil {
		parent.ModTime = time.Now()
	}
	return idx.saveLocked()
}

// uploadPlain 无加密情况下的上传与索引更新。
func (idx *webDAVIndex) uploadPlain(ctx context.Context, name, remotePath, tempPath string, file *os.File, size int64) error {
	readHandle, err := os.Open(tempPath)
	if err != nil {
		return err
	}
	result, err := idx.backend.PutObject(ctx, remotePath, readHandle)
	_ = readHandle.Close()
	if err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	cacheName := cacheNameForPath(name)
	cachePath := filepath.Join(idx.cacheDir, cacheName)
	if err := replaceFile(tempPath, cachePath); err != nil {
		return err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.Entries[name] = &webDAVIndexEntry{
		Path:       name,
		RemotePath: remotePath,
		Key:        result.Key,
		IsDir:      false,
		Size:       size,
		ModTime:    time.Now(),
		FileURL:    result.FileURL,
		SourceURL:  result.SourceURL,
		CacheName:  cacheName,
	}
	if parent := idx.Entries[parentWebDAVPath(name)]; parent != nil {
		parent.ModTime = time.Now()
	}
	return idx.saveLocked()
}

func (idx *webDAVIndex) removeAll(ctx context.Context, name string) error {
	cleaned, err := cleanWebDAVPath(name)
	if err != nil {
		return err
	}
	if cleaned == "/" {
		return fmt.Errorf("不能删除根目录")
	}

	idx.mu.Lock()
	entries := idx.entriesUnderLocked(cleaned)
	idx.mu.Unlock()

	if len(entries) == 0 {
		return os.ErrNotExist
	}

	for _, entry := range entries {
		if entry.IsDir || entry.RemotePath == "" {
			continue
		}
		if err := idx.backend.ClearObject(ctx, entry.RemotePath); err != nil {
			return err
		}
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	for _, entry := range entries {
		delete(idx.Entries, entry.Path)
		if entry.CacheName != "" {
			_ = os.Remove(filepath.Join(idx.cacheDir, entry.CacheName))
		}
	}
	if parent := idx.Entries[parentWebDAVPath(cleaned)]; parent != nil {
		parent.ModTime = time.Now()
	}
	return idx.saveLocked()
}

func (idx *webDAVIndex) rename(ctx context.Context, oldName, newName string) error {
	oldPath, err := cleanWebDAVPath(oldName)
	if err != nil {
		return err
	}
	newPath, err := cleanWebDAVPath(newName)
	if err != nil {
		return err
	}
	if oldPath == "/" || newPath == "/" {
		return fmt.Errorf("不能移动根目录")
	}

	idx.mu.Lock()
	oldEntry := idx.Entries[oldPath]
	newEntry := idx.Entries[newPath]
	newParent := idx.Entries[parentWebDAVPath(newPath)]
	entries := idx.entriesUnderLocked(oldPath)
	idx.mu.Unlock()

	if oldEntry == nil || len(entries) == 0 {
		return os.ErrNotExist
	}
	if newEntry != nil {
		return os.ErrExist
	}
	if newParent == nil || !newParent.IsDir {
		return os.ErrNotExist
	}
	if oldEntry.IsDir && strings.HasPrefix(newPath+"/", oldPath+"/") {
		return fmt.Errorf("不能把目录移动到自身下面")
	}

	// 第一遍：把每个文件上传到新的扁平 key（同一 key 用于上传 + 记录到索引）。
	newFlats := make(map[string]string) // entry.Path -> 新 flat key
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		cachePath, err := idx.ensureCache(entry)
		if err != nil {
			return err
		}
		file, err := os.Open(cachePath)
		if err != nil {
			return err
		}
		newEntryPath := transformedPath(entry.Path, oldPath, newPath)
		realPath, err := remotePathFromDAVPath(newEntryPath)
		if err != nil {
			file.Close()
			return err
		}
		flatKey := flatRemoteKey(idx.username, realPath)
		if err := func() error {
			defer file.Close()
			_, err := idx.backend.PutObject(ctx, flatKey, file)
			return err
		}(); err != nil {
			return err
		}
		newFlats[entry.Path] = flatKey
	}

	for _, entry := range entries {
		if entry.IsDir || entry.RemotePath == "" {
			continue
		}
		if err := idx.backend.ClearObject(ctx, entry.RemotePath); err != nil {
			return err
		}
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	for _, entry := range entries {
		delete(idx.Entries, entry.Path)
	}
	for _, entry := range entries {
		moved := entry.clone()
		moved.Path = transformedPath(entry.Path, oldPath, newPath)
		if !moved.IsDir {
			flatKey := newFlats[entry.Path]
			moved.RemotePath = flatKey
			moved.Key = obsObjectRoot + "/" + flatKey
		}
		moved.ModTime = time.Now()
		idx.Entries[moved.Path] = moved
	}
	if parent := idx.Entries[parentWebDAVPath(oldPath)]; parent != nil {
		parent.ModTime = time.Now()
	}
	if parent := idx.Entries[parentWebDAVPath(newPath)]; parent != nil {
		parent.ModTime = time.Now()
	}
	return idx.saveLocked()
}

func transformedPath(current, oldRoot, newRoot string) string {
	if current == oldRoot {
		return newRoot
	}
	return newRoot + strings.TrimPrefix(current, oldRoot)
}

func (idx *webDAVIndex) entriesUnderLocked(root string) []*webDAVIndexEntry {
	entries := make([]*webDAVIndexEntry, 0)
	for entryPath, entry := range idx.Entries {
		if entryPath == root || strings.HasPrefix(entryPath, root+"/") {
			entries = append(entries, entry.clone())
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
	return entries
}

func (idx *webDAVIndex) entry(name string) (*webDAVIndexEntry, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	entry, ok := idx.Entries[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return entry.clone(), nil
}

func (idx *webDAVIndex) ensureCache(entry *webDAVIndexEntry) (string, error) {
	if entry.CacheName == "" {
		return "", os.ErrNotExist
	}
	cachePath := filepath.Join(idx.cacheDir, entry.CacheName)
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	if entry.SourceURL == "" && entry.FileURL == "" {
		return "", os.ErrNotExist
	}

	remoteURL := entry.SourceURL
	if remoteURL == "" {
		remoteURL = entry.FileURL
	}
	if err := idx.downloadCache(remoteURL, cachePath); err != nil {
		return "", err
	}
	return cachePath, nil
}

func (idx *webDAVIndex) downloadCache(remoteURL, cachePath string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(remoteURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("回填缓存失败: %s", resp.Status)
	}

	if err := os.MkdirAll(idx.cacheDir, 0755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(idx.cacheDir, "webdav-download-*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	_, copyErr := io.Copy(tempFile, resp.Body)
	closeErr := tempFile.Close()
	if copyErr != nil {
		os.Remove(tempName)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tempName)
		return closeErr
	}
	return replaceFile(tempName, cachePath)
}

func (idx *webDAVIndex) saveLocked() error {
	data, err := json.MarshalIndent(struct {
		Entries map[string]*webDAVIndexEntry `json:"entries"`
	}{Entries: idx.Entries}, "", "  ")
	if err != nil {
		return err
	}

	// 索引用 DEK 整体加密后落盘，防止本机/远端读到明文元数据。
	var out []byte
	if idx.dek != nil {
		out, err = aeadSeal(idx.dek, data)
		if err != nil {
			return err
		}
	} else {
		out = data
	}

	if err := os.MkdirAll(filepath.Dir(idx.indexPath), 0755); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(filepath.Dir(idx.indexPath), "webdav-index-*.json")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()
	if _, err := tempFile.Write(out); err != nil {
		tempFile.Close()
		os.Remove(tempName)
		return err
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempName)
		return err
	}
	return replaceFile(tempName, idx.indexPath)
}

func replaceFile(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return closeErr
	}
	return os.Remove(src)
}

func (entry *webDAVIndexEntry) clone() *webDAVIndexEntry {
	if entry == nil {
		return nil
	}
	cloned := *entry
	return &cloned
}

func (entry *webDAVIndexEntry) fileInfo() os.FileInfo {
	name := path.Base(entry.Path)
	if entry.Path == "/" {
		name = "/"
	}
	modTime := entry.ModTime
	if modTime.IsZero() {
		modTime = time.Now()
	}
	return webDAVFileInfo{
		name:    name,
		size:    entry.Size,
		modTime: modTime,
		isDir:   entry.IsDir,
	}
}

type webDAVFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (info webDAVFileInfo) Name() string       { return info.name }
func (info webDAVFileInfo) Size() int64        { return info.size }
func (info webDAVFileInfo) ModTime() time.Time { return info.modTime }
func (info webDAVFileInfo) IsDir() bool        { return info.isDir }
func (info webDAVFileInfo) Sys() any           { return nil }

func (info webDAVFileInfo) Mode() os.FileMode {
	if info.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
