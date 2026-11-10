# Mailroom

Go + SQLite + 原生 HTML/CSS/JavaScript 的本地邮件整理应用。Gmail 新邮件 → OpenAI 分类 → Gmail 标签 → 人工或 Codex 复核。

## 快速运行演示

需要 Go 1.26.10+（当前依赖的最低 Go 版本以 go.mod 为准）。

```sh
go build -o bin/mailroom ./cmd/mailroom
MAILROOM_DEMO=1 ./bin/mailroom
```

打开 **http://127.0.0.1:8787**。演示包含六封合成邮件；不调用 Google、OpenAI 或 Codex。可以体验搜索、分类依据、人工复核、归档、垃圾箱和恢复。演示和真实模式默认使用不同数据目录；不允许混用。

## 连接真实邮箱

1. 在 Google Cloud 项目启用 Gmail API。
2. 配置 OAuth consent。个人 Gmail 使用 External；测试阶段把自己的账号添加为测试用户。
3. 创建 **Desktop app** OAuth 客户端，下载 `credentials.json`。
4. 选择你账号可用且支持 Responses API Structured Outputs 的模型。
5. 设置环境变量并启动：

```sh
export GOOGLE_CREDENTIALS='/absolute/path/credentials.json'
export OPENAI_API_KEY='your-key'
export OPENAI_MODEL='your-available-model'
./bin/mailroom
```

页面「连接与设置」→「连接 Google 账户」。授权回调默认是 `http://127.0.0.1:8787/oauth/callback`，使用随机 state、PKCE 和浏览器状态校验。请用 127.0.0.1 打开页面，不要改成 localhost。

应用请求 `gmail.modify`，用于读邮件、加标签、归档和移入垃圾箱。这个权限本身也包含发送能力，但本应用没有发送接口，也没有永久删除接口。Google 测试应用的 refresh token 可能过期；重新授权即可，已有本地处理结果不丢失。

首次连接只建立监控起点，**不自动扫描历史邮箱**。每 120 秒使用 Gmail History 增量同步；历史游标失效时按监控起点补查。页面可手动导入最近 7 天。每轮最多分类 5 封，逐封调用模型；失败持久化后指数退避重试。消息 ID 去重，自己的标签变化不重复触发分类。

## 标签与保留规则

- `AI/推广`：普通推广、资讯、产品更新；模型评分 ≥ 0.95 且无保护因素。
- `AI/保留`：账单、收据、订单退款、合同、私人和工作往来、医疗、税务、法律、安全、账户及政策变更。会话中实际发出的回复强制保留。
- `AI/待复核`：低评分、混合或不完整内容、需要附件等。

模型评分不是准确率。分类结果是建议；默认只加标签，不归档、不删邮件、不改变已读状态。附件不上传给模型；正文不足或附件需要解释时交给人工。标签中的推广也可人工改为保留。

归档/垃圾箱由页面逐封操作，垃圾箱操作有确认；支持恢复到收件箱。操作只作用于消息 ID，不整条会话修改。外部请求与本地提交无法组成单一事务，因此清理前保存动作意图；失败时保留记录，可重试同一动作完成核验。恢复只支持本应用记录的操作，不能恢复 Google 已永久删除的邮件。

## MCP 与 Codex

同一个二进制提供 MCP stdio 服务：

```sh
./bin/mailroom mcp
```

先运行 Web 后台，MCP 通过本地 HTTP 接口连接它。MCP 提供四个工具：

- `list_pending`：最多 20 封待复核邮件。
- `get_email`：缓存正文、分类依据及实时会话上下文。
- `complete_review`：记录保留/推广结论和依据。
- `apply_action`：归档、垃圾箱、恢复；默认拒绝 agent 清理。

外部 Codex MCP 配置示例（路径改成自己的）：

```toml
[mcp_servers.mailroom]
command = "/absolute/path/mailroom/bin/mailroom"
args = ["mcp"]

[mcp_servers.mailroom.env]
MAILROOM_DATA_DIR = "/absolute/path/to/mailroom-data"
MAILROOM_ADDR = "127.0.0.1:8787"
```

`MAILROOM_DATA_DIR` 必须与后台完全相同。stdio 的 stdout 只用于协议。MCP token 位于数据目录，权限 0600；后台重启后 MCP 每次调用重新读取 token。

页面「启动一轮复核」会启动本机已安装并登录的 `codex app-server`，创建/恢复**独立的 Mailroom 会话**，通过 `turn/start` 让 Codex 使用上述 MCP 工具。它不会接管桌面中现有聊天。默认复核指令不允许发送、归档、垃圾箱或恢复操作；服务器请求交互审批时会拒绝，留下邮件供人工处理。每轮最多复核 20 封，10 分钟超时，5 分钟冷却；进程断开不删除待办。

设 `MAILROOM_AUTO_CODEX=1` 可在同步成功后自动调度待复核队列。Codex 使用你本机的登录和配置；它的其他本地工具由你的 Codex 配置管理。Mailroom 对自己的 MCP 动作执行单独权限校验。`MAILROOM_CODEX_ACTIONS=1` 才允许 agent 对已经确认的、非受保护推广邮件使用清理工具；默认调度任务依然只复核。不要把网络或邮件内容当作额外操作授权。

## 配置

| 变量 | 默认/用途 |
|---|---|
| `MAILROOM_ADDR` | `127.0.0.1:8787`，仅允许 loopback |
| `MAILROOM_DATA_DIR` | 系统用户配置目录下 `mailroom` 或 `mailroom-demo` |
| `GOOGLE_CREDENTIALS` | `credentials.json` |
| `OPENAI_API_KEY` | 必填，真实模式分类使用 |
| `OPENAI_MODEL` | 必填，账户可用且支持结构化输出的模型 |
| `MAILROOM_POLL_SECONDS` | `120`，最小 `30` |
| `MAILROOM_DEMO` | `1` 开启合成数据演示 |
| `MAILROOM_AUTO_CODEX` | `1` 开启自动复核 |
| `MAILROOM_CODEX_ACTIONS` | `1` 允许 MCP 指定清理操作 |
| `CODEX_BINARY` | `codex` |

`.env.example` 仅为参考；程序不会自动读取 `.env`。一个数据目录只供一个后台进程使用。数据库和 Google token 存在本地，文件权限受限但没有应用层加密。不要提交数据库或凭据。原始 HTML 不执行、不加载邮件追踪图片，页面使用纯文本显示邮件。模型调用使用 `store:false`，但这不等于 API 零保留承诺。

## 测试与构建

```sh
go test -race ./...
go vet ./...
go build -o bin/mailroom ./cmd/mailroom
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/mailroom-linux-amd64 ./cmd/mailroom
```

测试覆盖 SQLite 持久化、跨账号/演示隔离、正文提取、保留规则、结构化输出失败、Gmail 分页/重复事件/游标过期、标签/垃圾箱/恢复、读状态保留、HTTP Host/Origin/CSRF、真实 MCP stdio 往返和 Codex App Server 协议流程（模拟子进程）。真实 Google OAuth 和真实 OpenAI/Codex 推理需要用户配置与登录，未在无凭据环境里伪造通过。

Linux 上运行相同二进制对应构建；远程访问可使用 SSH 转发：`ssh -L 8787:127.0.0.1:8787 your-server`。OAuth 的本机回调也通过此转发到服务。

## 目录

- `cmd/mailroom`：Web 后台与 MCP 入口。
- `internal/app/gmail.go`：OAuth、Gmail 增量同步和 MIME。
- `internal/app/classifier.go`：Responses API 与保留规则。
- `internal/app/engine.go`：队列、重试、标签与动作。
- `internal/app/store.go`：SQLite 状态和审计日志。
- `internal/app/http.go`：本机 Web API、安全边界和授权回调。
- `internal/app/mcp.go` / `codex.go`：MCP 工具与 Codex 调度。
- `internal/app/web`：无需前端构建工具的页面。

参考：[Gmail 同步](https://developers.google.com/workspace/gmail/api/guides/sync)、[Google OAuth](https://developers.google.com/workspace/gmail/api/quickstart/go)、[OpenAI Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs)、[Codex App Server](https://learn.chatgpt.com/docs/app-server)。
