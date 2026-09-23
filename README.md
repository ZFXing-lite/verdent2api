# verdent2api

把 [Verdent](https://www.verdent.ai/zh-CN) **桌面端 Free mode 限免模型**封装成 OpenAI 兼容 API。

不是控制台 team API key。限免模型只走桌面端真实链路：

`PKCE 登录 → Bearer token → AES-256-GCM 加密信封 → llm-proxy.verdent.ai/llm/stream`

本项目把这条链路翻译成 `/v1/chat/completions`，任何 OpenAI 客户端都能直接用。

> 仅限本人授权账号、本机/私有环境。遵守 Verdent 服务条款，不要对外收费转售。

📖 [部署文档](docs/部署文档.md) · [使用文档](docs/使用文档.md)

## 能做什么

- **限免模型**：`/v1/models` 默认只列 Free mode（`-free` 后缀或 `is_limit_free`），带 `is_free: true`
- **OpenAI 兼容**：`/v1/chat/completions`（流式 / 非流式）、`/v1/models`、`/status`、`/healthz`
- **桌面端协议**：请求头、加密信封、system 指纹模板与桌面 App 对齐；客户端 system 折进消息，不覆盖被指纹的 `body.system`
- **多账号轮换**：429 / 限免窗口用尽自动冷却换号，401 且无法刷新则禁用
- **热重载**：登录写入新账号后，网关 15 秒内自动进池，不用重启
- 纯 Go，零第三方依赖

内嵌兜底目录目前包含已确认的限免模型：

- `deepseek-v4.1-flash-free`
- `glm-5.3-flash-free`

本机装过 Verdent 桌面版时，会优先读 `~/.verdent/model-catalog-cache.json`，限免列表随缓存更新。`catalog_file` 也可以指向一份自己维护的目录。

## 快速开始

```bash
./build.sh
./bin/verdent-login                 # 浏览器登录，token 写入 auths/accounts.json
cp config.example.json config.json  # 把 api_key 改成随机长串
./bin/verdent-server -config config.json
```

```bash
curl http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer 你的网关密钥" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

客户端 Base URL 填 `http://127.0.0.1:7866/v1`。

## 协议要点

| 项 | 值 |
|---|---|
| 上游 | `https://llm-proxy.verdent.ai/llm/stream` |
| 登录 | PKCE：`www.verdent.ai/auth` → 本地回调 → `login.verdent.ai/passport/pkce/callback` |
| 刷新 | `POST login.verdent.ai/passport/token/refresh` |
| 加密 | AES-256-GCM，信封 `base64(nonce12 \| ciphertext \| tag)`，加密 `messages` / `tools` |
| 身份头 | `verdent-proxy-beta: hybrid-stream@20250919`，`X-Version-Code: 2.15.1` |
| 响应 | Anthropic 风格 hybrid-stream，本地翻译成 OpenAI chunk |

App 更新 agent prompt 后如果开始稳定 429（错误码 20004），需要重新抓一份 `template.json` 替换 `internal/verdent/template.json` 再编译。

## 测试

```bash
go test ./...
```

覆盖加密信封与 Python 参考实现对拍、请求体加密与 system 折叠、hybrid-stream 翻译、限免目录过滤。
