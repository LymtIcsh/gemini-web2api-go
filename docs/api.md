# API 使用指南

日期：2026-11-03；执行者：Codex。

以下示例使用服务地址 `http://127.0.0.1:8083/v1`。`YOUR_SERVICE_API_KEY` 指管理面板里的 API Key，不是管理密码或 Google API Key。先按 [README](../README.md) 启动服务并配置账号。

## 模型与健康检查

```bash
curl http://127.0.0.1:8083/
curl http://127.0.0.1:8083/v1/models -H "Authorization: Bearer YOUR_SERVICE_API_KEY"
```

PowerShell 中使用 `curl.exe`，避免旧版 PowerShell 的 `curl` 别名。健康接口只说明服务进程就绪；模型列表也不能证明账号具备全部模型权限。

## Python SDK：对话与流式

安装：`python -m pip install openai`。

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8083/v1",
    api_key="YOUR_SERVICE_API_KEY",
)
stream = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{"role": "user", "content": "Explain how rainbows form."}],
    stream=True,
)
for chunk in stream:
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="", flush=True)
print()
```

普通对话按上游增量转发。带 `tools` 的请求需要完整文本解析工具调用；`reasoning_content` 是否返回取决于模型和实际上游响应。

## Responses

```python
response = client.responses.create(
    model="gemini-3.6-flash",
    input="Explain how rainbows form.",
)
print(response.output_text)
```

Responses 支持兼容事件格式，但 `stream=True` 在生成完成后才发送事件，不是真正的逐增量上游转发。

## 图片输入

```python
import base64
from pathlib import Path

image = base64.b64encode(Path("example.png").read_bytes()).decode("ascii")
response = client.chat.completions.create(
    model="gemini-3.6-flash",
    messages=[{
        "role": "user",
        "content": [
            {"type": "text", "text": "Describe this image."},
            {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{image}"}},
        ],
    }],
)
print(response.choices[0].message.content)
```

需要可用登录态，也支持 HTTP(S) 图片链接。图片 MIME 必须与内容对应。图片、音乐等网页生成通过对话提示词使用，没有独立的 `/v1/images/*` 或 `/v1/audio/*` 接口。

## 异步视频

视频生成取决于账号是否实际拥有上游视频能力。下面用标准库展示创建、轮询和下载，等待上限为 15 分钟。

```python
import json
import time
from pathlib import Path
from urllib.request import Request, urlopen

base = "http://127.0.0.1:8083/v1"
key = "YOUR_SERVICE_API_KEY"

def request(path, payload=None, raw=False):
    body = None if payload is None else json.dumps(payload).encode("utf-8")
    req = Request(base + path, data=body, headers={
        "Authorization": f"Bearer {key}",
        "Content-Type": "application/json",
    })
    with urlopen(req, timeout=60) as response:
        data = response.read()
    return data if raw else json.loads(data)

job = request("/videos", {
    "prompt": "A blue wooden cube slowly rotating on a white table.",
    "size": "1280x720",
})
deadline = time.monotonic() + 900
while time.monotonic() < deadline:
    job = request(f"/videos/{job['id']}")
    if job["status"] == "failed":
        raise RuntimeError(job.get("error", job))
    if job["status"] == "completed":
        Path("output.mp4").write_bytes(request(f"/videos/{job['id']}/content", raw=True))
        print("Saved output.mp4")
        break
    time.sleep(3)
else:
    raise TimeoutError("Video is still pending; keep the job ID to query again.")
```

| 操作 | 请求 |
| --- | --- |
| 创建 | `POST /v1/videos`，JSON 包含 `prompt` |
| 查询 | `GET /v1/videos/{id}`，状态为 `queued`、`in_progress`、`completed` 或 `failed` |
| 下载 | `GET /v1/videos/{id}/content`，完成后返回 MP4 |
| 续写 | `POST /v1/videos/{id}/extend`，JSON 包含后续 `prompt`，返回新任务 |

创建时可以加 `input_reference`（data/HTTP(S) URL 或图片对象），也支持 multipart 图片上传。尺寸支持 `1280x720` / `720x1280`，或 `aspect_ratio` 为 `16:9` / `9:16`。续写沿用原账号和会话。

任务当前存于进程内存，重启后不可继续按旧 ID 查询。容量不足、额度耗尽和上游拒绝应读取任务错误，不能当作已生成视频；客户端轮询超时本身不会取消服务端任务。

## MCP 与客户端配置

兼容客户端填写 Base URL `http://127.0.0.1:8083/v1`、服务 API Key 和模型名即可。MCP HTTP 地址为 `http://127.0.0.1:8083/mcp`，携带相同 Bearer Key，提供 `web_search`。

工具调用采用提示词解析，不能保证强制调用或任意客户端完整兼容。采样参数没有对应的网页端控制；具体边界见 [README](../README.md#api-与限制)。
