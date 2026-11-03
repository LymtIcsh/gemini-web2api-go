# 发布前验证

日期：2026-11-03；执行者：Codex。

以下结果来自当前源码在 Windows 本机的自动执行，不依赖 GitHub Actions 或远程 CI。

| 检查 | 命令 | 结果 |
| --- | --- | --- |
| Go 全量回归 | `go test -count=1 ./...` | 通过，`internal/app` 3.484 秒 |
| Go 静态检查 | `go vet ./...` | 通过，退出码 0 |
| 源码构建 | `go build -trimpath -o .codex/publish-gemini-web2api-go.exe .` | 通过，退出码 0；程序版本 4.22.0 |
| Node 全量回归 | 在 `browser-worker` 中执行 `npm test` | 117 通过、0 失败、0 跳过 |
| 真实浏览器冒烟 | 设置 `BROWSER_REAL_SMOKE=1` 后执行 `npm run smoke` | 通过，退出码 0 |

## 本次修复

首次 Node 全量运行发现 4 个并发刷新子用例失败：另一账号刷新导致表格重绘，迟到的额度响应仍尝试修改已移除的旧行；随后列表读取失败时，当前行保留过期数值。恢复按账号编号定位当前有效行的逻辑后，同一套测试全部通过。测试保留 HTTP 200/409、列表 HTTP 503/网络断线、固定选择及页面离开等断言。

## 验证范围

- Go 回归覆盖服务接口、账号调度、额度准入、存储、浏览器通信及媒体恢复等本地行为；实际启用条件以各测试为准。
- Node 测试包含真实 Firefox 管理页交互、额度解析、临时目录和工作进程生命周期，外部业务请求使用受控输入。
- 真实 Camoufox 冒烟使用临时档案与本地 HTTP 服务，验证流式字节、取消隔离、下载、双档案隔离、默认单槽切换、空闲释放及 Cookie/LocalStorage/指纹恢复。

本轮没有访问 Google 账号，也没有验证真实 Gemini 文本、图片、音乐或视频生成；不能由本地通过推断在线能力。Docker、Linux/macOS、真实 MySQL/PostgreSQL 服务未在本轮部署验证。现有 Dockerfile 仅构建 Go 服务，不含浏览器运行环境。

复现步骤见 [README](../README.md#开发与验证)。完整本机输出保留在被忽略的 `.codex/publish-*` 文件中，不分发账号资料或运行日志。
