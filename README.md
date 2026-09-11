# U-Drive CLI

U-Drive CLI 是一个命令行工具，用于管理平台的文件上传和删除操作。

## 功能特点

- 文件上传：支持将本地文件上传到平台
  - 实时显示上传进度条
  - 显示上传速度和文件大小
  - 美观的进度条动画效果
- 文件删除：支持删除已上传的文件
- 自动认证：自动处理登录认证和 token 管理
- 缓存支持：自动缓存认证 token，提高使用效率

## 安装

### 从源码安装

1. 确保已安装 Go 环境（推荐 Go 1.16 或更高版本）
2. 克隆仓库：
   ```bash
   git clone https://github.com/yourusername/u-drive-cli.git
   cd u-drive-cli
   ```
3. 编译安装：
   ```bash
   go build -o udrive
   ```
4. 将可执行文件移动到系统路径：
   ```bash
   sudo mv udrive /usr/local/bin/
   ```

## 使用方法

### 查看帮助

```bash
udrive -h
# 或
udrive --help
```

### 上传文件

```bash
udrive upload <filename>
```

例如：
```bash
udrive upload test.jpg
```

上传时会显示实时进度条：
```
正在上传文件... [===============>----------------] 45% | 4.5MB/10MB | 2.3MB/s
```

### 删除文件

```bash
udrive delete <filename>
```

例如：
```bash
udrive delete test.jpg
```

## WebDAV（基于 uLearning 内容 API）

`udrive webdav` 启动一个 WebDAV 服务器，把 uLearning 内容库当作网盘挂载。**目录树是真实的远端数据**：由 uLearning 内容 API 承载，不再是本地索引；两台设备同时挂载看到的是同一份远端目录，无需同步任何索引。文件内容仍以 AES-256-GCM 流式加密后存到 OBS（凭证即密钥，改密码旧数据不可读）。

多人使用：**一个 `udrive webdav` 服务器可服务多个用户，启动零账号**，所有用户共用同一个后端上传账号。客户端挂载时填的 Basic Auth `username`/`password` 即该用户的身份，纯函数派生出两样东西：

1. **根目录名** `udrive-<hash16>`（`sha256(username+password)` 前 16 个十六进制字符）：在内容库里每人一个独立目录，彼此的文件互相不可见；
2. **加密密钥**（PBKDF2-HMAC-SHA256）：加密文件内容。

只要账号密码正确，任何机器都能算出同一密钥解密自己的文件——**无需备份任何 key / keyring**。

### 启动

```bash
udrive webdav --addr 127.0.0.1 --port 8080 --prefix /dav
```

参数：

- `--addr` 监听地址，默认 `127.0.0.1`，避免暴露到公网
- `--port` 监听端口，默认 `8080`
- `--prefix` 挂载前缀，默认 `/dav`
- `--no-auth` 禁用 Basic Auth，仅供本机调试

### 客户端连接

- macOS Finder：Go → 连接服务器 → `http://127.0.0.1:8080/dav`，输入用户名密码
- Windows 网络位置：`\\127.0.0.1@8080\DavWWWRoot\dav`（或地图网络驱动器）
- Cyberduck / rclone / davfs2：URL 填 `http://127.0.0.1:8080/dav`

curl 示例（以 alice/secret 登录，账号即加密身份）

```bash
# 列表
curl -u alice:secret -X PROPFIND -H 'Depth: 1' http://127.0.0.1:8080/dav/
# 建目录
curl -u alice:secret -X MKCOL http://127.0.0.1:8080/dav/test/
# 上传
curl -u alice:secret -T README.md http://127.0.0.1:8080/dav/test/README.md
# 下载
curl -u alice:secret http://127.0.0.1:8080/dav/test/README.md
# 复制 / 移动 / 删除
curl -u alice:secret -X COPY -H 'Destination: http://127.0.0.1:8080/dav/test/README-copy.md' http://127.0.0.1:8080/dav/test/README.md
curl -u alice:secret -X MOVE -H 'Destination: http://127.0.0.1:8080/dav/test/README-moved.md' http://127.0.0.1:8080/dav/test/README-copy.md
curl -u alice:secret -X DELETE http://127.0.0.1:8080/dav/test/README.md
```

### 重要限制（务必知晓）

1. **目录列表有最长 15 秒解析缓存**：另一台设备上的改动可能延迟可见（写操作会立即失效自己会话的缓存）。
2. **改密码 = 新密钥，旧加密文件不可读**（凭证即密钥）。改密码前请自行导出旧数据。
3. **OBS 上传 token 只允许单段 key**（路径含 `/` 会 403），因此对象是扁平命名 `<roothash>-<时间戳>.zip`，目录结构只存在于内容 API 记录中。
4. **MOVE/复制**：跨目录移动通过「新建记录复用同一 location + 删除旧记录」实现，不搬字节。
5. **上传仍受后端单次上传 5GB 限制**。
6. **明文密码走 HTTP**：监听非 `127.0.0.1` 时建议配 TLS。
7. **内容 API 的记录里文件名/大小是明文元数据**，只有内容字节是密文。

## 注意事项

1. 首次使用时需要登录认证，认证信息会被缓存
2. 文件上传后会自动生成可访问的 URL
3. 删除操作不可逆，请谨慎操作
4. 支持的文件类型取决于平台的限制
5. 上传大文件时会显示实时进度条，方便监控上传状态
6. 登录账号密码可通过环境变量 `UDRIVE_LOGIN_NAME` / `UDRIVE_PASSWORD` 覆盖

## 错误处理

如果遇到错误，程序会显示详细的错误信息，包括：
- 认证失败
- 文件操作失败
- 网络连接问题
- 其他系统错误

## 贡献

欢迎提交 Issue 和 Pull Request 来帮助改进这个项目。

## 许可证

MIT License 