# 浏览器工作进程

Node ESM JSON Lines 工作进程。每个 `profile_id` 使用独立的 Camoufox 持久档案，Go 负责账号选择、业务协议、超时和重试；真实请求在账号浏览器页面中执行。

## 安装与启动

要求 Node >=22.15；官方 TypeScript 客户端为 `camoufox`，其 peer dependency 要求 `playwright-core <1.63`。

```powershell
cd browser-worker
npm ci
npx camoufox fetch
$env:BROWSER_PROFILE_ROOT = 'D:\GeminiPool\data\browser-profiles'
node worker.mjs
```

当前项目依赖固定为 `camoufox 0.5.8`、`playwright-core 1.62.1`，使用现有 `package-lock.json` 安装。生产库按需 import，因此纯 IPC 与本地测试不要求启动浏览器。诊断写入 stderr，stdout 仅输出协议。

## 内存和并发

日期：2026-10-25；执行者：Codex。

| 环境变量 | 默认值 | 行为 |
| --- | --- | --- |
| `BROWSER_MAX_BROWSERS` | `1` | 限制同时驻留的浏览器实例；必须是正整数 |
| `BROWSER_IDLE_TIMEOUT_MS` | `60000` | 后台浏览器空闲后释放；`0` 关闭空闲计时，容量限制仍有效 |

账号档案数量独立于浏览器实例数量。切换账号时优先关闭没有活动请求的后台浏览器，之后从该账号的磁盘档案恢复。所有槽位忙时请求排队，取消和总超时会结束等待。

活动请求和用户打开的登录窗口保留。默认单槽下，用户窗口占用槽位期间，其他账号需要等待窗口关闭。空闲释放保留磁盘上的 Cookie、LocalStorage 和 `identity.json`；下一次请求重新启动浏览器，冷启动会增加等待时间。提高 `BROWSER_MAX_BROWSERS` 可以增加账号并发，也会增加内存。

## 协议

stdin 每行一个请求：

```json
{"id":"job-1","account_id":1,"op":"request","profile_id":"account-1","expected_email":"owner@example.com","proxy_url":"","timeout_ms":0,"url":"https://gemini.google.com/app","method":"GET","headers":{}}
```

支持 `login`、`confirm-login`、`refresh`、`quota`、`open`、`close`、`request`、`download`、`cancel`、`shutdown`。请求 body 使用 `body_base64`；取消使用 `target_id`。`timeout_ms` 由 Go 提供；`quota` 在省略或为 0 时仍有默认总超时，其他操作不额外设超时。

`expected_email` 是 Go 已绑定的账号邮箱。Google request/download 必须提供并匹配真实验证身份；缺少绑定或身份未验证返回 `login_required`，邮箱不符返回 `account_mismatch`，两者 session 均为 needs_login。校验在生成/上传 fetch 或媒体导航之前完成；认证 Cookie 在验证后、发送前变化也停止发送。已绑定的 login/open/confirm-login/refresh 同样检查；初次未绑定的登录可不传 expected_email，返回实际邮箱供 Go 绑定。

stdout startup 发 `{"id":"","account_id":0,"type":"ready"}`。请求按 ID 并发分派，响应为 `start`（`status,headers,url`）→ `chunk`（`data_base64`）→ `end`；失败为 `error`（`code,error`）。每个事件保留 `id/account_id`。控制操作只返回 `end`，登录操作附加 `session`：`{email,cookie,login_state,profile_id}`；未验证登录的 `confirm-login/refresh` 返回 `error(code=login_required,session.login_state=needs_login)`。

Google request/download 的 `end` 也返回 `session`，在浏览器处理响应 Cookie 后重新读取完整 Cookie 快照供 Go 更新。请求使用自己的 Gemini 准备页面核实身份；下载优先复用此前已核实的页面身份，缺少身份时才新增临时确认页。调用方提供的 `SAPISIDHASH` 在页面导航后按当前浏览器 Cookie 重新计算，避免 Cookie 自动更新造成授权头与会话不一致。

已核实身份缓存同时绑定稳定认证 Cookie（SAPISID、SID 和对应 1P/3P 认证 Cookie）。认证 Cookie 变化或 context 重开后重新检查 Gemini；SIDCC 票据轮转只刷新快照。检查期间认证 Cookie 再次变化时返回空邮箱和 needs_login，避免把旧邮箱与其他账号 Cookie 配成 ready。

已有 Gemini 页面 DOM 只在认证 Cookie 与页面核实时相同才可继续用；请求响应或可见登录页面认证改变后，在独立页面重新导航 Gemini 检查，避免旧 DOM 邮箱/token 被当作新 Cookie 身份。浏览器 context 关闭事件清空该实例缓存，下次操作重开保存的档案身份，不影响其他账号或重置活动任务计数。

每次 Gemini 身份导航在 goto 前捕获认证 Cookie key；导航后、读取身份后都必须仍匹配原 key。导航期间认证变化会返回 needs_login（初次可见登录为 logging_in），Google 生成/上传/媒体请求不会发送，需要后续重新确认。

`login/open` 使用可见模式；普通 request/download 使用后台模式。模式或代理变化只在档案没有活动任务时关闭重开；否则返回 `profile_busy`。`close` 同样拒绝活动档案。取消只关闭对应请求页面，shutdown 关闭本进程全部自有 contexts。

## 登录与档案

档案根默认 `<cwd>/data/browser-profiles`，可通过 `BROWSER_PROFILE_ROOT` 设置。每个档案首次调用官方 `launchOptions`，约束为实际操作系统，把生成的 `CAMOU_CONFIG_*`、浏览器路径、参数与 preferences 写入 `identity.json`；后续启动直接复用，模式和代理不参与身份生成。浏览器数据保存在该目录下的 `browser/`；不删除用户档案。

工作进程不填写密码，也不采用调用方传入的邮箱标签。确认登录使用独立准备页面，检查 Gemini origin、Google 账号控件的唯一邮箱、`SNlM0e` token 与浏览器认证 Cookie。浏览器持久档案保存完整 Cookie（domain/path/expiry/删除语义）；返回的 Cookie 字符串仅是 Go 内部快照。实际邮箱比较由 Go 执行。页面结构或 Google 登录流程变化需要实际验证，HTML 200 不代表已登录。

请求通过页面 `fetch(credentials:'include')`、ReadableStream 和 awaited `exposeBinding` 传递原始字节，业务头保留、浏览器管理头剔除。Google 服务请求包括上传 endpoint 都从 Gemini origin 发出；页面 CORS 生效。不会使用 Node fetch、APIRequestContext 或 route.fetch 发送生产上游请求。

媒体先使用原生浏览器导航。附件下载使用 Playwright Download 保存本进程临时文件，再用文件流分块转发，完成或取消后清理自有临时文件。附件状态和 MIME 必须来自原生 response 事件；缺少元数据返回 `download_metadata_unavailable`，不会推定 200。非附件媒体在导航后的媒体同源页面使用 fetch 流重新读取，返回其实际 HTTP 状态/MIME；不累积 `response.body()`。这些 Firefox 导航/CORS 行为尚需真实 smoke 验证。

取消附件同时调用原生 `Download.cancel()`，防止保存和删除等待后台下载。非附件流使用导航最终响应 URL，跨源重定向后仍从最终媒体源读取。用户登录页有操作归属；并发 login/open 返回 `profile_busy`，取消读取既有用户页的操作仅中断该任务等待，不关闭之前打开的页面。

## 本地验证

```powershell
node --test test/*.test.mjs
$env:BROWSER_REAL_SMOKE = '1'
node smoke.mjs
```

纯 Node 测试只在浏览器边界注入可控设备，生产档案、调度、IPC、页面函数和文件流不替换。真实 smoke 默认拒绝执行；显式启用后使用独立临时档案和本地 HTTP 服务验证存储恢复、流式字节、双槽并发取消、附件下载，以及默认单槽淘汰和空闲释放后的 Cookie/LocalStorage/指纹恢复。空闲测试使用 2000ms，正式默认值仍为 60000ms。测试不访问 Google，不操作用户已打开的浏览器。

本地 HTTP smoke 只能证明浏览器生命周期及本地请求，不能代替真实 Gemini 业务验收。发布前本地验证见 [验证记录](../docs/verification.md)；本机诊断档案不随仓库分发。

## 官方资料

- https://github.com/daijro/camoufox/blob/main/typescript/README.md
- https://github.com/daijro/camoufox/blob/main/typescript/src/fingerprints.ts
- https://playwright.dev/docs/api/class-browsertype#browser-type-launch-persistent-context
- https://playwright.dev/docs/api/class-download
- https://playwright.dev/docs/api/class-page#page-expose-binding
