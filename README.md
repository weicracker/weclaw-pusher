# weclaw-pusher

微信消息推送和接收工具 - 通过 iLink API 与微信交互。

## 功能特性

- **多账号支持**: 支持同时登录多个微信账号
- **命令行发送**: 通过 CLI 发送文本和媒体消息
- **HTTP API**: 提供 REST API 接口用于发送和接收消息
- **消息接收**: 支持 Webhook 方式接收微信消息
  - **PULL 模式**: 第三方轮询调用 API
  - **PUSH 模式**: 收到消息自动 POST 到回调 URL（支持多回调广播）
- **扫码登录**: 支持添加微信账号
- **媒体支持**: 支持发送图片、视频、文件
- **Markdown 支持**: 文本消息支持 Markdown 语法

## 安装

```bash
go install github.com/fastclaw-ai/weclaw-pusher@latest
```

## 快速开始

### 1. 登录微信账号

```bash
weclaw-pusher login
```

会显示二维码，用微信扫描确认即可。重复登录可添加多个账号。

### 2. 命令行发送消息

`send` 命令独立工作，不需要启动 HTTP API 服务器（`serve` 命令）。它直接从本地凭证文件读取已登录账号，直接连接微信服务器发送消息。

```bash
# 发送文本
weclaw-pusher send --to "user_id@im.wechat" --text "Hello"

# 发送图片
weclaw-pusher send --to "user_id@im.wechat" --media "https://example.com/image.png"
```

### 3. HTTP API 发送消息

启动服务后（默认 `0.0.0.0:18011`），可以通过 HTTP 接口发送消息：

```bash
curl -X POST http://localhost:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{
    "to": "user_id@im.wechat",
    "text": "你好，这是一条测试消息"
  }'
```

发送图片：
```bash
curl -X POST http://localhost:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{
    "to": "user_id@im.wechat",
    "text": "查看这张图片",
    "media_url": "https://example.com/image.jpg"
  }'
```

健康检查：
```bash
curl http://localhost:18011/health
```

### 4. 接收微信消息

#### PULL 模式（轮询）

```bash
# 阻塞模式 (默认): 等待消息
weclaw-pusher listen

# 非阻塞模式: 立即返回队列中的消息
weclaw-pusher listen --non-blocking
```

获取消息:
```bash
curl http://127.0.0.1:18012/api/webhook
```

#### PUSH 模式（回调钩子）

```bash
# 单回调: 收到消息后自动 POST 到一个 URL
weclaw-pusher listen --callback-url http://your-server.com/webhook

# 多回调广播: 收到消息同时 POST 到多个 URL（逗号分隔）
weclaw-pusher listen --callback-url "http://hook1.com,http://hook2.com,http://hook3.com"

# 多账号独立回调: 每个账号的消息推送到不同的 URL
weclaw-pusher listen --account-callback "0=http://hook1.com,1=http://hook2.com"
```

## 多账号使用

### 登录多个账号

重复执行 `login` 命令即可添加多个微信账号：

```bash
weclaw-pusher login  # 第一个账号
weclaw-pusher login  # 第二个账号
```

### serve 模式（API 发送）

启动一个服务即可服务所有账号：

```bash
weclaw-pusher serve
```

**路由机制：** 用户先给机器人发消息 → 系统自动建立"用户 ↔ 账号"映射 → 之后 API 调用时根据 `to` 参数自动选择正确的账号发送。

```bash
# 回复用户A（系统自动选择接收过A消息的账号）
curl -X POST http://localhost:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "用户A的ID", "text": "hello"}'

# 回复用户B（系统自动选择接收过B消息的账号）
curl -X POST http://localhost:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "用户B的ID", "text": "hello"}'
```

**注意：** 目标用户必须先给机器人发一条消息，这样系统才能知道该用户属于哪个微信账号。

### listen 模式（接收消息）

每个账号都启动独立的 Monitor 监听消息：

```bash
# 所有账号共用一个回调（消息中包含 bot_id 标识来源）
weclaw-pusher listen --callback-url "http://hook1.com,http://hook2.com"

# 每个账号独立回调
weclaw-pusher listen --account-callback "0=http://hook1.com,1=http://hook2.com"
```

## 消息格式

```json
{
  "from": "user_id@im.wechat",
  "to": "bot_id@im.bot",
  "type": 1,
  "text": "用户发送的消息内容",
  "time": "2026-04-16T09:00:00Z",
  "bot_id": "账号ID（多账号时标识消息来源）"
}
```

## 命令

| 命令 | 描述 |
|------|------|
| `login` | 添加微信账号（扫码登录） |
| `send` | 发送消息到微信用户 |
| `serve` | 启动 HTTP API 服务器（发送消息，需要后台运行） |
| `listen` | 启动 Webhook 服务器（接收消息） |

## 打包编译

### 从源码编译

```bash
git clone https://github.com/fastclaw-ai/weclaw-pusher.git
cd weclaw-pusher
go build -o weclaw-pusher .
```

### 发布版本编译（去除调试信息）

```bash
go build -ldflags="-s -w" -o weclaw-pusher .
```

### 交叉编译

#### macOS (Intel & Apple Silicon)

```bash
# Intel
GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o weclaw-pusher-darwin-amd64 .

# Apple Silicon
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o weclaw-pusher-darwin-arm64 .
```

#### Linux

```bash
# AMD64
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o weclaw-pusher-linux-amd64 .

# ARM64
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o weclaw-pusher-linux-arm64 .
```

#### Windows

```bash
# AMD64
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o weclaw-pusher-windows-amd64.exe .
```

#### 一键全平台打包脚本

```bash
#!/bin/bash

VERSION=${1:-latest}
OUTPUT_DIR=dist

mkdir -p $OUTPUT_DIR

# macOS
GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o $OUTPUT_DIR/weclaw-pusher-darwin-amd64 .
GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o $OUTPUT_DIR/weclaw-pusher-darwin-arm64 .

# Linux
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $OUTPUT_DIR/weclaw-pusher-linux-amd64 .
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $OUTPUT_DIR/weclaw-pusher-linux-arm64 .

# Windows
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o $OUTPUT_DIR/weclaw-pusher-windows-amd64.exe .

echo "Build complete: $OUTPUT_DIR/"
ls -lh $OUTPUT_DIR
```

### 编译参数说明

| 参数 | 说明 |
|------|------|
| `-s` | 去除符号表（strip symbols） |
| `-w` | 去除 DWARF 调试信息 |
| `-ldflags` | 设置链接器标志 |

### 使用 GoReleaser 打包

项目根目录创建 `.goreleaser.yml`:

```yaml
before:
  hooks:
    - go mod download

builds:
  - binary: weclaw-pusher
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64

archives:
  - format: tarball
    format_overrides:
      - goos: windows
        format: zip
```

执行打包:

```bash
goreleaser build --clean --snapshot --output dist/
```

## listen 命令选项

| 选项 | 说明 |
|------|------|
| `--listen-addr` | Webhook API 监听地址（默认 127.0.0.1:18012） |
| `--non-blocking, -n` | 非阻塞模式，立即返回 |
| `--timeout` | 阻塞超时（秒，0=无限） |
| `--callback-url` | 回调 URL，PUSH 模式（逗号分隔多个） |
| `--account-callback` | 每个账号独立的回调 URL（格式：0=http://url1,1=http://url2） |

## 配置

配置文件位于 `~/.weclaw/config.json`:

```json
{
  "api_addr": "127.0.0.1:18011"
}
```

账号凭证存储在 `~/.weclaw/accounts/` 目录。每个账号的凭证存储为独立的 JSON 文件。

## 与 weclaw 的区别

| 功能 | weclaw | weclaw-pusher |
|------|--------|---------------|
| 消息接收 | ✅ 支持 | ✅ 支持 |
| 主动发送 | ✅ 支持 | ✅ 支持 |
| AI Agent 回复 | ✅ 支持 | ❌ 不支持 |
| 扫码登录 | ✅ 支持 | ✅ 支持 |

## License

MIT