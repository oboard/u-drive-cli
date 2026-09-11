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

## WebDAV（upload-only hack，多用户加密）

`udrive` 可以通过 WebDAV 协议把本地文件当作挂载盘使用。**注意：这是一个 upload-only 的 hack 方案，不是完整真实的远端文件系统**，原因是后端只签发 upload（且可覆盖）权限，不具备 list/get/delete/copy 权限。

多人使用：**一个 `udrive webdav` 服务器可服务多个用户，账号密码在登录时提供**（启动无需预配）。客户端挂载时填的 `username` 是用户唯一 ID、`password` 用于加密。所有用户**共用**同一个后端 OBS 账号，靠「扁平 key + 加密」互相隔离；OBS 对象、本地索引、缓存都是**密文**，每个用户的数据按 `username` 分目录（`~/.cache/udrive/<username>/`），彼此看不到内容。

> 后端 upload token 只授权往 `resources/web/<单个文件名>` 上传，**路径含 `/` 会 403**（实测）。因此每个文件被摊平成**单段扁平 key**（无 `/`）：`resources/web/<username>-<WebDAV路径以连字符连接>-<时间戳>.zip`。WebDAV 客户端看到的仍是真实目录树（由本地索引还原），只是 OBS 上的对象是扁平命名。

### 加密模型（凭证即密钥）

- 加密密钥 = `KDF(username, password)`（PBKDF2-HMAC-SHA256），**纯函数派生**。
- 任何机器，只要账号密码正确，就能算出同一密钥解密——**无需备份任何 key / keyring**。
- **文件内容、本地缓存、索引都用该密钥加密**（AES-256-GCM，防篡改）。
- 代价：**改密码会派生新密钥，旧密文将不可读**（不提供自动迁移，需自行导出旧数据）。

### 启动

```bash
udrive webdav --addr 127.0.0.1 --port 8080 --prefix /dav
```

启动零配置、零账号。客户端连接时填的 username/password 即为该用户的加密密钥与隔离标识；不同账号登录同一 server 各自独立。

参数：

- `--addr` 监听地址，默认 `127.0.0.1`，避免暴露到公网
- `--port` 监听端口，默认 `8080`
- `--prefix` 挂载前缀，默认 `/dav`
- `--data-dir` 各用户索引/缓存根目录（默认 `~/.cache/udrive`，其下按 `<username>/` 分目录）
- `--no-auth` 禁用 Basic Auth，仅供本机调试

### 客户端连接

- macOS Finder：Go → 连接服务器 → `http://127.0.0.1:8080/dav`，输入用户名密码
- Windows 网络位置：`\\127.0.0.1@8080\DavWWWRoot\dav`（或地图网络驱动器）
- Cyberduck / rclone / davfs2：URL 填 `http://127.0.0.1:8080/dav`

curl 示例（以 alice/secret 登录该多用户 server，账号即加密身份）

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

> 注意：真正让其他用户 **无法读取** 你内容的关键是 **加密**（OBS 对象、本地索引、缓存都是密文），`Basic Auth` 只是进入 server 的门槛；错误密码派生的密钥解不开密文。

### 重要限制（务必知晓）

1. **列表来自本地索引，不代表 OBS 远端真实内容**：只对通过这个 WebDAV server 上传/创建过的文件可见。换机器需带上本机 `~/.cache/udrive/<username>/` 下的索引才能恢复列表（加密内容用账号密码可解，但文件清单需要索引）。
2. **删除只是远端清空为 0 字节，不是真删除**：受限于只有 upload 权限，`DELETE` 只能上传 0 字节覆盖远端对象，无法真正删除对象。
3. **改密码会派生新密钥，旧密文不可读**：这是「凭证即密钥」的必然代价。密码 = 解密钥匙；改密码前请自行导出旧数据。若误改，旧文件无法用新密码解密。
4. **每个用户在 OBS 的扁平 key 前缀隔离（`resources/web/<username>-...`），内容密文**：其他用户即使拿到对象也读不了明文（需各自账号密码派生的密钥）。
5. **空目录只保存在本地索引**，不会真的建到远端。
6. **MOVE/COPY 依赖本地缓存**：如果本地缓存丢失，会尝试用索引里的 URL 回填；两者都不可用时可能失败。
7. **上传仍受后端单次上传大小限制**，且依赖本地临时文件和缓存磁盘空间。
8. 只适用于本机 / 可信网络；请务必开启 Basic Auth，不要把没有认证的实例暴露到局域网/公网。明文密码走网络传输，若监听非 `127.0.0.1` 建议配 TLS。

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