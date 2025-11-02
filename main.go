package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
	"github.com/schollz/progressbar/v3"
)

type ObsToken struct {
	AK            string `json:"ak"`
	SK            string `json:"sk"`
	SecurityToken string `json:"securitytoken"`
	Bucket        string `json:"bucket"`
	Endpoint      string `json:"endpoint"`
	Domain        string `json:"domain"`
}

type ApiResponse struct {
	Result ObsToken `json:"result"`
}

type LoginRequest struct {
	LoginName  string `json:"loginName"`
	Password   string `json:"password"`
	Device     string `json:"device"`
	AppVersion string `json:"appVersion"`
	WebEnv     string `json:"webEnv"`
}

type LoginResponse struct {
	Token string `json:"token"`
}

type TokenCache struct {
	Token      string    `json:"token"`
	ExpireTime time.Time `json:"expireTime"`
}

func getToken() (string, error) {
	// 检查缓存文件是否存在
	if _, err := os.Stat("cache.json"); err == nil {
		// 读取缓存文件
		data, err := os.ReadFile("cache.json")
		if err != nil {
			return "", err
		}

		var cache TokenCache
		if err := json.Unmarshal(data, &cache); err != nil {
			return "", err
		}

		// 检查token是否过期
		if time.Now().Before(cache.ExpireTime) {
			return cache.Token, nil
		}
	}

	// 如果缓存不存在或已过期，重新登录获取token
	loginData := url.Values{}
	loginData.Set("loginName", "oboard10@teml.net")
	loginData.Set("password", "Aa123456")

	// 创建一个 cookie jar
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", fmt.Errorf("创建 cookie jar 失败: %v", err)
	}

	// 创建一个允许重定向的客户端，并设置 cookie jar
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
	}

	// 创建请求
	req, err := http.NewRequest("POST", "https://courseapi.ulearning.cn/users/login/v2", strings.NewReader(loginData.Encode()))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("发送请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 获取所有 cookies
	cookies := jar.Cookies(req.URL)

	// 从 cookie jar 中获取 token
	var token string
	for _, cookie := range cookies {
		if cookie.Name == "token" {
			token = cookie.Value
			break
		}
	}

	if token == "" {
		// 如果没有找到 token，尝试从 AUTHORIZATION cookie 获取
		for _, cookie := range cookies {
			if cookie.Name == "AUTHORIZATION" {
				token = cookie.Value
				break
			}
		}
	}

	if token == "" {
		// 构建详细的错误信息
		return "", fmt.Errorf("未找到 token cookie (状态码: %d, 响应头: %v, Cookies: %v)",
			resp.StatusCode,
			resp.Header,
			cookies)
	}

	// 缓存token，设置1小时过期
	cache := TokenCache{
		Token:      token,
		ExpireTime: time.Now().Add(time.Hour),
	}

	cacheData, err := json.Marshal(cache)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile("cache.json", cacheData, 0644); err != nil {
		return "", err
	}

	return token, nil
}

func getUploadToken(filename, token string) (*ObsToken, error) {
	url := fmt.Sprintf("https://courseapi.ulearning.cn/obs/uploadToken?path=resources/web/%s", filename)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var apiResp ApiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}

	return &apiResp.Result, nil
}

// uploadToObs 处理文件上传到OBS的核心逻辑
func uploadToObs(reader io.Reader, filename string) (string, string, error) {
	// 获取文件大小
	var fileSize int64
	if seeker, ok := reader.(io.Seeker); ok {
		// 保存当前位置
		currentPos, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return "", "", fmt.Errorf("获取文件位置失败: %v", err)
		}
		// 获取文件大小
		fileSize, err = seeker.Seek(0, io.SeekEnd)
		if err != nil {
			return "", "", fmt.Errorf("获取文件大小失败: %v", err)
		}
		// 恢复位置
		_, err = seeker.Seek(currentPos, io.SeekStart)
		if err != nil {
			return "", "", fmt.Errorf("恢复文件位置失败: %v", err)
		}
	}

	// 检查文件大小是否超过5GB
	if fileSize > 5*1024*1024*1024 {
		return "", "", fmt.Errorf("文件大小超过5GB限制")
	}

	// 创建进度条
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

	// 创建带进度条的读取器
	progressReader := progressbar.NewReader(reader, bar)

	// 获取token
	token, err := getToken()
	if err != nil {
		return "", "", fmt.Errorf("获取token失败: %v", err)
	}

	// 获取上传凭证
	obsToken, err := getUploadToken(filename, token)
	if err != nil {
		return "", "", fmt.Errorf("获取上传凭证失败: %v", err)
	}

	// 创建OBS客户端
	obsClient, err := obs.New(obsToken.AK, obsToken.SK, obsToken.Endpoint, obs.WithSecurityToken(obsToken.SecurityToken))
	if err != nil {
		return "", "", fmt.Errorf("创建OBS客户端失败: %v", err)
	}

	// 创建上传输入
	input := &obs.PutObjectInput{}
	input.Bucket = obsToken.Bucket
	input.Key = fmt.Sprintf("resources/web/%s", filepath.Base(filename))
	input.Body = &progressReader

	// 执行上传
	_, err = obsClient.PutObject(input)
	if err != nil {
		return "", "", fmt.Errorf("上传文件失败: %v", err)
	}

	// 构建文件访问地址
	fileUrl := fmt.Sprintf("%s/%s", obsToken.Domain, input.Key)
	sourceUrl := fmt.Sprintf("https://leicloud-huawei.obs.cn-north-4.myhuaweicloud.com/%s", input.Key)
	return fileUrl, sourceUrl, nil
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST 请求", http.StatusMethodNotAllowed)
		return
	}

	// 设置最大文件大小为 4GB
	r.Body = http.MaxBytesReader(w, r.Body, 4<<30)

	// 从请求中获取文件
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, fmt.Sprintf("获取文件失败: %v", err), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 获取远程文件名参数
	remoteName := r.FormValue("remotename")
	filename := header.Filename
	if remoteName != "" {
		filename = remoteName
	}

	// 创建临时文件
	tempFile, err := os.CreateTemp("", "udrive-*")
	if err != nil {
		http.Error(w, fmt.Sprintf("创建临时文件失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// 将上传的文件复制到临时文件
	if _, saveErr := io.Copy(tempFile, file); saveErr != nil {
		http.Error(w, fmt.Sprintf("保存文件失败: %v", saveErr), http.StatusInternalServerError)
		return
	}

	// 重置文件指针到开始位置
	if _, seekErr := tempFile.Seek(0, 0); seekErr != nil {
		http.Error(w, fmt.Sprintf("重置文件指针失败: %v", seekErr), http.StatusInternalServerError)
		return
	}

	// 上传文件
	fileUrl, sourceUrl, err := uploadToObs(tempFile, filename)
	if err != nil {
		http.Error(w, fmt.Sprintf("上传文件失败: %v", err), http.StatusInternalServerError)
		return
	}

	// 返回成功响应
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message":    "文件上传成功",
		"file":       header.Filename,
		"remoteName": filename,
		"sourceUrl":  sourceUrl,
		"cdnUrl":     fileUrl,
	})
}

func deleteFile(filename string) error {
	// 创建空文件内容
	emptyContent := bytes.NewReader([]byte{})

	// 上传空文件（覆盖原文件）
	_, _, err := uploadToObs(emptyContent, filename)
	if err != nil {
		return fmt.Errorf("删除文件失败: %v", err)
	}

	fmt.Printf("文件已清空！\n")
	return nil
}

func startServer(port int) error {
	http.HandleFunc("/upload", handleUpload)

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("服务器启动在 http://localhost%s\n", addr)
	fmt.Printf("使用 POST 请求上传文件，例如：\n")
	fmt.Printf("curl -X POST -F \"file=@your_file.txt\" http://localhost%s/upload\n", addr)
	fmt.Printf("指定远程文件名：\n")
	fmt.Printf("curl -X POST -F \"file=@your_file.txt\" -F \"remotename=new_name.txt\" http://localhost%s/upload\n", addr)

	return http.ListenAndServe(addr, nil)
}

func main() {
	// 定义命令行参数
	uploadCmd := flag.NewFlagSet("upload", flag.ExitOnError)
	deleteCmd := flag.NewFlagSet("delete", flag.ExitOnError)
	serveCmd := flag.NewFlagSet("serve", flag.ExitOnError)

	// 定义serve命令的端口参数
	port := serveCmd.Int("port", 8080, "服务器端口号")

	// 定义upload命令的重命名参数
	remoteName := uploadCmd.String("r", "", "上传后的文件名")

	// 检查命令行参数
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Println("使用方法: udrive <command> [arguments]")
		fmt.Println("可用命令:")
		fmt.Println("  upload <filename>    上传文件")
		fmt.Println("    -r <remotename>    指定上传后的文件名")
		fmt.Println("  delete <filename>    删除文件")
		fmt.Println("  serve --port PORT    启动HTTP服务器")
		fmt.Println("    可通过POST请求参数remotename指定上传后的文件名")
		fmt.Println("\n选项:")
		fmt.Println("  -h, --help           显示帮助信息")
		os.Exit(1)
	}

	// 解析命令
	switch os.Args[1] {
	case "upload":
		uploadCmd.Parse(os.Args[2:])
		if uploadCmd.NArg() != 1 {
			fmt.Println("错误: 请指定要上传的文件名")
			fmt.Println("使用方法: udrive upload [-r 远程文件名] <filename>")
			os.Exit(1)
		}
		filename := uploadCmd.Arg(0)
		file, err := os.Open(filename)
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		defer file.Close()

		// 使用指定的远程文件名或本地文件名
		remoteFilename := filename
		if *remoteName != "" {
			remoteFilename = *remoteName
		}

		fileUrl, sourceUrl, err := uploadToObs(file, remoteFilename)
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("文件上传成功！\n")
		fmt.Printf("源地址（华为云）: %s\n", sourceUrl)
		fmt.Printf("CDN地址（带缓存）: %s\n", fileUrl)

	case "delete":
		deleteCmd.Parse(os.Args[2:])
		if deleteCmd.NArg() != 1 {
			fmt.Println("错误: 请指定要删除的文件名")
			fmt.Println("使用方法: udrive delete <filename>")
			os.Exit(1)
		}
		filename := deleteCmd.Arg(0)
		if err := deleteFile(filename); err != nil {
			fmt.Printf("错误: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		serveCmd.Parse(os.Args[2:])
		if err := startServer(*port); err != nil {
			log.Fatalf("服务器启动失败: %v", err)
		}
	default:
		fmt.Printf("未知命令: %s\n", os.Args[1])
		fmt.Println("使用方法: udrive <command> [arguments]")
		fmt.Println("可用命令:")
		fmt.Println("  upload <filename>    上传文件")
		fmt.Println("    -r <remotename>    指定上传后的文件名")
		fmt.Println("  delete <filename>    删除文件")
		fmt.Println("  serve --port PORT    启动HTTP服务器")
		fmt.Println("    可通过POST请求参数remotename指定上传后的文件名")
		os.Exit(1)
	}
}
