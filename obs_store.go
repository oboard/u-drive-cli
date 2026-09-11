package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
	"github.com/schollz/progressbar/v3"
)

const obsObjectRoot = "resources/web"
const maxUploadSize = 5 * 1024 * 1024 * 1024

type obsUploadResult struct {
	FileURL   string
	SourceURL string
	Key       string
}

func normalizeRemotePath(name string) (string, error) {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" {
		return "", fmt.Errorf("远程路径不能为空")
	}
	if strings.Contains(name, "\x00") {
		return "", fmt.Errorf("远程路径包含非法字符")
	}

	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("远程路径不能包含 ..")
		}
	}

	cleaned := path.Clean("/" + name)
	if cleaned == "/" {
		return "", fmt.Errorf("远程路径不能为空")
	}
	return strings.TrimPrefix(cleaned, "/"), nil
}

func objectKeyForRemotePath(name string) (string, error) {
	remotePath, err := normalizeRemotePath(name)
	if err != nil {
		return "", err
	}
	return obsObjectRoot + "/" + remotePath, nil
}

// flatRemoteKey 把真实路径 + username 编码成**单段（无 "/"）**的扁平 OBS key。
//
// 后端 upload token 只授权往 `resources/web/<单个文件名>` 上传，路径中一旦出现 "/" 会 403
// （实测）。所以把目录段压成连字符、加时间戳避免同名互相覆盖，并统一加 `.zip` 后缀。
//
// 例：username=alice, 真实路径 dir/foo.txt -> `alice-dir-foo.txt-<milliTs>.zip`
func flatRemoteKey(username, realPath string) string {
	flat := strings.ReplaceAll(realPath, "/", "-")
	return username + "-" + flat + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10) + ".zip"
}

func uploadReaderToObs(reader io.Reader, filename string, showProgress bool) (*obsUploadResult, error) {
	fileSize, err := readerSize(reader)
	if err != nil {
		return nil, err
	}
	if fileSize > maxUploadSize {
		return nil, fmt.Errorf("文件大小超过5GB限制")
	}

	body := reader
	if showProgress {
		bar := progressbar.NewOptions64(
			fileSize,
			progressbar.OptionSetDescription("正在上传文件..."),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "=",
				SaucerHead:    ">",
				SaucerPadding: " ",
				BarStart:      "[",
				BarEnd:        "]",
			}),
			progressbar.OptionShowBytes(true),
			progressbar.OptionSetWidth(50),
			progressbar.OptionThrottle(100*time.Millisecond),
			progressbar.OptionShowCount(),
			progressbar.OptionOnCompletion(func() {
				fmt.Fprint(os.Stderr, "\n")
			}),
		)
		progressReader := progressbar.NewReader(reader, bar)
		body = &progressReader
	}

	key, err := objectKeyForRemotePath(filename)
	if err != nil {
		return nil, err
	}

	token, err := getToken()
	if err != nil {
		return nil, fmt.Errorf("获取token失败: %v", err)
	}

	obsToken, err := getUploadToken(filename, token)
	if err != nil {
		return nil, fmt.Errorf("获取上传凭证失败: %v", err)
	}

	obsClient, err := obs.New(obsToken.AK, obsToken.SK, obsToken.Endpoint, obs.WithSecurityToken(obsToken.SecurityToken))
	if err != nil {
		return nil, fmt.Errorf("创建OBS客户端失败: %v", err)
	}

	input := &obs.PutObjectInput{}
	input.Bucket = obsToken.Bucket
	input.Key = key
	input.Body = body

	_, err = obsClient.PutObject(input)
	if err != nil {
		return nil, fmt.Errorf("上传文件失败: %v", err)
	}

	return &obsUploadResult{
		FileURL:   fmt.Sprintf("%s/%s", obsToken.Domain, input.Key),
		SourceURL: fmt.Sprintf("https://leicloud-huawei.obs.cn-north-4.myhuaweicloud.com/%s", input.Key),
		Key:       input.Key,
	}, nil
}

func readerSize(reader io.Reader) (int64, error) {
	seeker, ok := reader.(io.Seeker)
	if !ok {
		return 0, nil
	}

	currentPos, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, fmt.Errorf("获取文件位置失败: %v", err)
	}

	fileSize, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("获取文件大小失败: %v", err)
	}

	_, err = seeker.Seek(currentPos, io.SeekStart)
	if err != nil {
		return 0, fmt.Errorf("恢复文件位置失败: %v", err)
	}

	return fileSize, nil
}

func uploadToObsNoProgress(reader io.Reader, filename string) (string, string, error) {
	result, err := uploadReaderToObs(reader, filename, false)
	if err != nil {
		return "", "", err
	}
	return result.FileURL, result.SourceURL, nil
}

func clearRemoteObject(filename string) error {
	_, _, err := uploadToObsNoProgress(bytes.NewReader(nil), filename)
	return err
}
