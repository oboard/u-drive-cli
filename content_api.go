package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// content_api.go — uLearning 内容 API 客户端。
//
// 用途：WebDAV 的文件/目录「元数据与目录结构」全部由 uLearning 的 content API
// 承载（参考 /Users/oboard/Development/u-drive 前端），代替之前的本地 index。
// 文件字节仍在 OBS（单段扁平 key），记录里的 location 指向 OBS 对象。
//
// 所有 WebDAV 用户共用一个后端账号（getToken()），多用户隔离靠
// Basic Auth 凭据派生的「hash 根目录」。

const contentAPIBase = "https://courseapi.ulearning.cn"

// contentType: 1=本地上传(文件)  3=文件夹
const (
	contentTypeFile   = 1
	contentTypeFolder = 3
	contentStatusOK   = 2
)

// contentFileInfo 对应前端 models/list_info.ts 的 FileInfo。
type contentFileInfo struct {
	ContentID   int64           `json:"contentId"`
	ParentID    int64           `json:"parentID"` // 注意后端两种写法：parentID / parentId
	ParentIDAlt int64           `json:"parentId"` // 上传创建时用 parentId
	Title       string          `json:"title"`
	Type        int             `json:"type"` // 1 文件，3 文件夹
	MimeType    string          `json:"mimeType"`
	ContentSize int64           `json:"contentSize"`
	Location    string          `json:"location"`
	CreateDate  int64           `json:"createDate"`
	LastModDate int64           `json:"lastModDate"`
	Remark      string          `json:"remark"`
	Remark1     string          `json:"remark1"`
	Remark2     string          `json:"remark2"`
	Remark3     string          `json:"remark3"`
	Status      json.RawMessage `json:"status"` // 数字/字符串都可能
	IsView      string          `json:"isView"`
	IsDelete    int             `json:"isdelete"`
	Depth       int             `json:"depth"`
}

// parentIDOf 返回后端任意形态下的父 id。
func (m *contentFileInfo) parentIDOf() int64 {
	if m.ParentID != 0 {
		return m.ParentID
	}
	return m.ParentIDAlt
}

// isFolder 判断该记录是否为目录。
func (m *contentFileInfo) isFolder() bool { return m.Type == contentTypeFolder }

// modTime 返回记录的最后修改时间（毫秒时间戳）。
func (m *contentFileInfo) modTime() time.Time {
	if m.LastModDate > 0 {
		return time.UnixMilli(m.LastModDate)
	}
	if m.CreateDate > 0 {
		return time.UnixMilli(m.CreateDate)
	}
	return time.Now()
}

// contentListResponse 对应 ListInfo。
type contentListResponse struct {
	List []contentFileInfo `json:"list"`
}

// uploadContentRecord 对应前端 UploadFileInfo。
type uploadContentRecord struct {
	Title       string `json:"title"`
	Type        int    `json:"type"`
	Status      int    `json:"status"`
	ContentSize int64  `json:"contentSize"`
	Location    string `json:"location,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	IsView      int    `json:"isView"`
	Remark2     int    `json:"remark2"`
	Remark3     int    `json:"remark3"`
	ParentID    int64  `json:"parentId"`
}

// apiClient 抽象 uLearning content API（便于 fake 测试）。
type apiClient interface {
	ListChildren(parentID int64) ([]contentFileInfo, error)
	GetMeta(id int64) (*contentFileInfo, error)
	CreateFolder(title string, parentID int64) error
	CreateFile(rec uploadContentRecord) error
	UpdateFile(meta contentFileInfo, newTitle string, newParentID int64, newLocation string, newSize int64, newMime string) error
	DeleteContent(ids []int64) error
}

// httpContentAPI 走真实 HTTP。
type httpContentAPI struct {
	tokenProvider func() (string, error)
	httpClient    *http.Client
	baseURL       string
}

func newHTTPContentAPI(tokenProvider func() (string, error)) *httpContentAPI {
	return &httpContentAPI{
		tokenProvider: tokenProvider,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		baseURL:       contentAPIBase,
	}
}

// token 缓存，避免每个请求都走 getToken()。
func (c *httpContentAPI) token() (string, error) {
	return c.tokenProvider()
}

// doJSON 做 JSON 请求；token 失败时返回的能力要重试吗？MVP 不处理。
func (c *httpContentAPI) doJSON(method, url string, body any, out any) error {
	token, err := c.token()
	if err != nil {
		return fmt.Errorf("获取 token 失败: %v", err)
	}

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", token)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("API 返回 %s: %s", resp.Status, truncate(string(respBody), 200))
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("解析 API 响应失败: %v", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ListChildren 调用 content/user/list 列出 parentID 下的文件与目录。
func (c *httpContentAPI) ListChildren(parentID int64) ([]contentFileInfo, error) {
	url := fmt.Sprintf("%s/content/user/list?keyword=&parentId=%d&pn=1&ps=100000&viewType=0&lang=zh",
		c.baseURL, parentID)
	var out contentListResponse
	if err := c.doJSON("GET", url, nil, &out); err != nil {
		return nil, err
	}
	return out.List, nil
}

// GetMeta 调用 content/<id> 取单个记录；root(id=0) 返回空。
func (c *httpContentAPI) GetMeta(id int64) (*contentFileInfo, error) {
	url := fmt.Sprintf("%s/content/%d?lang=zh", c.baseURL, id)
	var out contentFileInfo
	if err := c.doJSON("GET", url, nil, &out); err != nil {
		return nil, err
	}
	if out.ContentID == 0 {
		return nil, nil
	}
	return &out, nil
}

// postRecord 调 course/content/upload 创建或更新记录。
func (c *httpContentAPI) postRecord(body any) error {
	url := c.baseURL + "/course/content/upload?lang=zh"
	return c.doJSON("POST", url, body, nil)
}

// CreateFolder 创建目录。
func (c *httpContentAPI) CreateFolder(title string, parentID int64) error {
	return c.postRecord(uploadContentRecord{
		Title:       title,
		Type:        contentTypeFolder,
		Status:      contentStatusOK,
		ContentSize: 0,
		IsView:      0,
		Remark2:     1,
		Remark3:     0,
		ParentID:    parentID,
	})
}

// CreateFile 创建文件记录。
func (c *httpContentAPI) CreateFile(rec uploadContentRecord) error {
	return c.postRecord(rec)
}

// UpdateFile 更新记录（改名 / 换目录 / 覆盖内容）。
func (c *httpContentAPI) UpdateFile(meta contentFileInfo, newTitle string, newParentID int64, newLocation string, newSize int64, newMime string) error {
	if newTitle == "" {
		newTitle = meta.Title
	}
	if newParentID == 0 {
		newParentID = meta.parentIDOf()
	}
	if newLocation == "" {
		newLocation = meta.Location
	}
	if newSize == 0 {
		newSize = meta.ContentSize
	}
	if newMime == "" {
		newMime = meta.MimeType
	}

	body := map[string]any{
		"contentId":   meta.ContentID,
		"parentId":    newParentID,
		"title":       newTitle,
		"type":        meta.Type,
		"mimeType":    newMime,
		"contentSize": newSize,
		"location":    newLocation,
		"status":      meta.Status,
		"isView":      meta.IsView,
		"remark":      meta.Remark,
		"remark1":     meta.Remark1,
		"remark2":     meta.Remark2,
		"remark3":     meta.Remark3,
		"createDate":  meta.CreateDate,
		"depth":       meta.Depth,
	}
	return c.postRecord(body)
}

// DeleteContent 删除一批 contentId。
func (c *httpContentAPI) DeleteContent(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	url := c.baseURL + "/content/delete?_method=DELETE&lang=zh"
	return c.doJSON("DELETE", url, ids, nil)
}

// flatOBSKey 生成 OBS 扁平 key（单段，无 "/"）。
// <用户hash>-<毫秒时间戳><.zip>
func flatOBSKey(userHex string, timestamp int64) string {
	return userHex + "-" + strconv.FormatInt(timestamp, 10) + ".zip"
}
