# weclaw-pusher

微信消息推送和接收工具 - 通过 iLink API 与微信交互。

## 功能特性

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

或从源码编译:

```bash
git clone https://github.com/fastclaw-ai/weclaw-pusher.git
cd weclaw-pusher
go build -o weclaw-pusher .
```

## 快速开始

### 1. 登录微信账号

```bash
weclaw-pusher login
```

会显示二维码，用微信扫描确认即可。

### 2. 命令行发送消息

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
```

## 消息格式

```json
{
  "from": "user_id@im.wechat",
  "to": "bot_id@im.bot",
  "type": 1,
  "text": "用户发送的消息内容",
  "time": "2026-04-16T09:00:00Z"
}
```

## 命令

| 命令 | 描述 |
|------|------|
| `login` | 添加微信账号（扫码登录） |
| `send` | 发送消息到微信用户 |
| `serve` | 启动 HTTP API 服务器（发送消息） |
| `listen` | 启动 Webhook 服务器（接收消息） |
| `stop` | 停止后台服务 |

## listen 命令选项

| 选项 | 说明 |
|------|------|
| `--listen-addr` | Webhook API 监听地址（默认 127.0.0.1:18012） |
| `--non-blocking, -n` | 非阻塞模式，立即返回 |
| `--timeout` | 阻塞超时（秒，0=无限） |
| `--callback-url` | 回调 URL，PUSH 模式（逗号分隔多个） |

## 配置

配置文件位于 `~/.weclaw/config.json`:

```json
{
  "api_addr": "127.0.0.1:18011"
}
```

账号凭证存储在 `~/.weclaw/accounts/` 目录。

## 与 weclaw 的区别

| 功能 | weclaw | weclaw-pusher |
|------|--------|---------------|
| 消息接收 | ✅ 支持 | ✅ 支持 |
| 主动发送 | ✅ 支持 | ✅ 支持 |
| AI Agent 回复 | ✅ 支持 | ❌ 不支持 |
| 扫码登录 | ✅ 支持 | ✅ 支持 |

## License

MIT