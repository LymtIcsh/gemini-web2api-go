# gemini-web2api-go

**将 Gemini 网页端接入 OpenAI 兼容客户端，自带浏览器账号池、额度管理和中文管理面板。**

[![Go](https://img.shields.io/badge/Go-1.26.8-00ADD8?logo=go)](go.mod)
[![Node.js](https://img.shields.io/badge/Node.js-%3E%3D22.15-339933?logo=nodedotjs)](browser-worker/package.json)
[![MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

中文 · [English](README_EN.md) · [API 使用](docs/api.md) · [浏览器工作进程](browser-worker/README.md) · [更新日志](CHANGELOG.md)

基于 [zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go)，保留上游历史与 MIT 许可证，加入浏览器账号、网页额度和 Windows 启动管理等扩展。使用 Gemini 网页协议，无需 Google API Key；模型、媒体能力和额度仍取决于实际 Google 账号与网页端开放情况。

## 功能

| 能力 | 说明 |
| --- | --- |
| OpenAI 兼容 | Chat Completions、Responses、模型列表；普通对话支持增量流式 |
| 浏览器账号池 | 每个账号独立 Camoufox 档案，网页登录、确认身份、刷新与批量管理 |
| 账号与额度调度 | 自动或固定账号，粘性或轮询策略，5 小时/周用量与恢复时间 |
| 多模态与视频 | 图片输入，网页图片/音乐生成，异步视频创建、下载、图生视频与续写 |
| 管理与存储 | 中英文面板、请求统计、代理池、运行时配置，SQLite / MySQL / PostgreSQL |

```text
OpenAI 兼容客户端 → Go API → Node + Camoufox → Gemini 网页端
                    └── 管理面板 / 账号 / 额度 / 存储
```

浏览器账号通过真实浏览器页面发送请求。源码也保留 Cookie 与匿名路径，可用性取决于上游；下面推荐使用浏览器账号。

## 快速开始

### Windows：双击启动

程序和依赖准备好后，日常使用只需：

1. 双击 **`start.bat`**。首次按提示设置至少 12 位管理密码，随后自动打开管理面板。
2. 在面板中添加并登录**自己的 Google 账号**，确认登录后即可使用。
3. 结束使用时双击 **`stop.bat`**。下次启动会保留本机配置和登录态。

面板：**http://127.0.0.1:8083/admin**。API Base URL：**http://127.0.0.1:8083/v1**。

仓库不包含作者的账号池数据、Cookie、浏览器登录态、API Key 或本机 `config.json`。首次运行生成自己的配置，账号需要自行添加。

<details>
<summary>第一次从 GitHub 下载源码？展开完成一次性准备</summary>

当前仓库提供源码，`start.bat` 不会自动编译或安装依赖。已准备好程序及依赖的用户可跳过本节。

安装 **Go 1.26.8 或兼容更新版本**、**Node.js ≥22.15** 和 Git。机器需能访问 Gemini，并具有可操作的桌面环境。

在 PowerShell 中执行：

```powershell
git clone https://github.com/LymtIcsh/gemini-web2api-go.git
cd gemini-web2api-go
cd browser-worker
npm ci
npx camoufox fetch
cd ..
go build -trimpath -o gemini-web2api-go.exe .
```

完成后双击根目录的 `start.bat`。请将生成的 EXE、`start.bat`、`stop.bat`、`launcher.ps1` 和 `browser-worker/` 保持在同一项目目录，Node 保持在 PATH 中。

启动器自动创建本机 `config.json`，已有配置不会覆盖；Go 服务自动启动浏览器工作进程。

</details>

<details>
<summary>Linux / macOS 与其他启动方式</summary>

Windows 启动时不自动打开面板：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\launcher.ps1 -Action Start -NoBrowser
```

Linux / macOS 安装上述 Go、Node 依赖及 Camoufox 后执行；网页登录需要桌面环境：

```bash
go build -trimpath -o gemini-web2api-go .
cp config.example.json config.json
# 编辑 host、admin_token 和 default_model
./gemini-web2api-go --config ./config.json
```

示例配置监听 `0.0.0.0`，仅本机使用请改为 `127.0.0.1`。Windows 首次配置默认模型为 `gemini-4-argon`，示例配置为 `gemini-3.6-flash`；请按账号权限在面板调整或在请求中指定模型。

</details>

### 登录账号并接入客户端

1. 打开面板，输入管理密码。
2. 在「账号池」添加账号，打开登录窗口，完成 Google 登录。
3. 点击「确认登录」，确认可用并刷新额度；完成后关闭可见窗口，释放默认的单浏览器槽位。
4. 获取面板中的服务 API Key，为客户端填写 Base URL、API Key 和模型。

**管理密码与 API Key 不同。** 未在启动时指定 API Key，服务会首次生成，可在面板查看和轮换。

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8083/v1",
    api_key="替换为面板中的 API Key",
)
response = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "用三句话介绍你自己。"}],
)
print(response.choices[0].message.content)
```

示例依赖：`python -m pip install openai`。流式、Responses、图片和视频示例见 [API 使用](docs/api.md)。

## 账号、额度与并发

- 默认 `sticky` 复用当前可用账号；`round_robin` 按请求轮换。也可固定账号。
- 5 小时或周窗口已用达到 100% 时退出自动池。到期后需网页明确恢复，不能仅靠倒计时恢复资格。
- 固定账号和既有会话保留原归属；人工禁用、登录失效与额度状态分别管理。
- 默认驻留一个浏览器，后台空闲 60 秒释放；可见登录窗口不会自动回收，其他账号可能等待它关闭。

刷新错误或“未知”不代表额度为零。增加浏览器数量可以提高并发，也会增加内存占用。

## 配置

示例见 [config.example.json](config.example.json)，完整字段见 [config.go](internal/app/config.go)。限流、重试、默认模型和选号策略可在面板调整。

| 配置 | 默认值 / 用途 |
| --- | --- |
| `host` / `port` | `0.0.0.0` / `8083`，Windows 首次配置监听 `127.0.0.1` |
| `db_path` | `./data/gemini.db` |
| `admin_token` | 管理密码，空值表示管理接口不鉴权 |
| `default_model` | `gemini-3.6-flash`，以配置与面板为准 |
| `account_strategy` | `sticky` 或 `round_robin` |
| `request_timeout_sec` | `180`，按任务耗时调整 |
| `proxy` | 启动时初始化代理池，后续通过面板管理 |

| 环境变量 | 用途 |
| --- | --- |
| `BROWSER_NODE` | Node 路径，默认 `node` |
| `BROWSER_WORKER_PATH` | 默认查找 `browser-worker/worker.mjs` |
| `BROWSER_PROFILE_ROOT` | Go 默认使用数据库目录下的 `browser-profiles` |
| `BROWSER_MAX_BROWSERS` | 驻留浏览器上限，默认 `1` |
| `BROWSER_IDLE_TIMEOUT_MS` | 默认 `60000`，`0` 关闭空闲计时 |
| `ADMIN_TOKEN` / `API_KEY` | 启动凭据，指定 API Key 后面板不能轮换 |
| `SQL_DSN` | MySQL / PostgreSQL 连接串，未设置使用 SQLite |

迁移账号时需一起保留数据库与浏览器档案。配置、账号数据、Cookie、日志和本机诊断资料不随源码上传。

## Docker 范围

当前 [Dockerfile](Dockerfile) 是 **仅含 Go 服务的 distroless 镜像**，没有 Node、Camoufox 或桌面环境，不能直接运行浏览器账号。完整功能请使用原生部署。

仅使用 Go 路径时可从当前源码构建：

```bash
docker compose up -d --build
```

[Compose](docker-compose.yml) 使用 `gemini-web2api-go:local` 和具名数据卷。上游镜像与 Releases 不包含本仓库扩展。

## API 与限制

| 接口 | 支持情况 |
| --- | --- |
| `POST /v1/chat/completions` | 文本、图片、普通对话真流式；tools 请求收集结果后解析 |
| `POST /v1/responses` | 兼容请求和事件，流式事件在生成完成后发送 |
| `GET /v1/models` | 当前暴露的模型，不保证账号拥有全部权限 |
| `/v1/videos` | 创建、查询、下载和续写，需要实际视频能力 |
| `/mcp` | HTTP MCP `web_search`，使用服务 API Key |
| `GET /` | 服务健康与版本，不代表 Google 登录或生成成功 |

模型以 `/v1/models` 为准，源码含 Flash、Pro、Argon 和 thinking 变体；实际路由和能力取决于上游。Function calling 为提示词解析，`tool_choice` 不保证强制执行。`temperature`、`top_p`、`max_tokens` 等没有对应网页控制。

embeddings、独立 `/v1/images/*`、`/v1/audio/*` 和 Gemini 原生 `/v1beta/*` 未实现。网页图片/音乐生成通过对话请求使用。

## 常见问题

| 现象 | 处理 |
| --- | --- |
| 找不到 Node 或 worker | 检查 `node --version`，将 `browser-worker/` 放在程序旁并安装依赖 |
| 找不到浏览器 | 在 worker 目录运行 `npx camoufox fetch` |
| 登录后仍不可用 | 点击确认登录，检查邮箱与账号绑定 |
| 切换账号或额度查询等待 | 关闭其他账号的可见窗口，默认只允许一个浏览器 |
| 额度未知或连接重置 | 查看错误，检查网络和代理后刷新 |
| 视频容量/额度不足 | 保留上游原因，等待能力恢复，不代表已生成视频 |
| 客户端 401 | 使用服务 API Key，Base URL 包含 `/v1` |

## 开发与验证

```bash
go test -count=1 ./...
go vet ./...
go build ./...
cd browser-worker
npm test
```

安装 Camoufox 后，在 worker 目录运行本地真实浏览器冒烟：

```powershell
$env:BROWSER_REAL_SMOKE = '1'
npm run smoke
```

Linux/macOS：`BROWSER_REAL_SMOKE=1 npm run smoke`。使用临时档案与本地 HTTP 服务，不访问 Google 或生产账号。实际结果见 [验证记录](docs/verification.md)。

```text
internal/app/       Go 接口、调度、存储和嵌入式面板
browser-worker/     Camoufox 工作进程、锁文件和 Node 测试
docs/              API 与验证说明
launcher.ps1       Windows 启停和打包
start.bat/stop.bat  Windows 启停入口
```

## 来源与许可

基于 [zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go)，浏览器部分使用 [Camoufox](https://github.com/daijro/camoufox) 与 [Playwright](https://playwright.dev/)。采用 [MIT License](LICENSE)，保留原有版权声明。

## 友情链接

[LINUX DO](https://linux.do/) — 感谢社区提供的技术交流与开源分享平台，认可并支持社区「真诚、友善、团结、专业」的理念。

文档整理：2026-11-03 · Codex。
