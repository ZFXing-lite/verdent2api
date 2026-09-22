# verdent2api

把 [Verdent](https://www.verdent.ai/zh-CN) 账号封装成 **OpenAI 兼容 API** 的多 key 网关：API key 池轮换、冷却/禁用状态机、流式透传。

架构沿用 [@Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 与 [@ZFXing-lite/autoclaw2api](https://github.com/ZFXing-lite/autoclaw2api) 的设计（凭证目录 + 池状态机 + OpenAI 协议适配），针对 Verdent 的实际协议做了适配。

> 仅限本人授权账号、本机/私有环境测试。请遵守 Verdent 服务条款，不要对外收费转售或滥用。

> 📖 详细文档：[部署文档](docs/部署文档.md) · [使用文档](docs/使用文档.md)

---

## 特性

- **OpenAI 兼容**：`/v1/chat/completions`（支持 `stream`）+ `/v1/models` + `/status` + `/healthz`
- **多 key 池**：加权随机选号、防惊群、在途租约上限、会话粘性（同一对话不跳号）
- **状态机**：`429` 短冷却 / 缺额长冷却 / 连续报错熔断 / `401` 鉴权失败自动禁用，全部落盘 `data/state.json`，重启不丢
- **登录工具**：本地渲染带 Cloudflare Turnstile 的登录页，`login` → 自动创建 API key → 写入凭证文件，全程不清空已有 key
- **key 热重载**：网关每 15s 重读凭证文件，登录写入新 key 后无需重启服务，近实时进池可用
- **故障转移**：单次请求首选 key 若在流开始前返回 429/5xx/网络错误，自动换下一个健康 key 重试（最多 3 个）
- **模型别名**：`model_alias` 把短名映射到上游真实模型名，并暴露进 `/v1/models`
- 纯 Go 标准库，零第三方依赖

## 逆向事实（全部来自官方前端，已实测）

| 项 | 值 |
|---|---|
| 官网 | `https://www.verdent.ai/zh-CN`（Next.js） |
| 云端 IDE | `https://cloud.verdent.ai`（React SPA，tRPC + WebSocket） |
| **上游 API** | `https://api.verdent.ai`，**标准 OpenAI 兼容**，`GET /v1/models`、`POST /v1/chat/completions`，`Authorization: Bearer sk-...` |
| 开发者控制台 | `https://platform.verdent.ai`（cookie 鉴权） |
| passport 登录域 | `https://login.verdent.ai` |
| 登录接口 | `POST /passport/login`，body `{email,password,token}`，`token` 为 Turnstile token |
| Turnstile sitekey | `0x4AAAAAABg0OrUFnV_hhAeU`（取自 cloud 前端常量 `Usn`） |
| 建键接口 | `POST /api/verdent/team/api-keys/create`，body `{name,team_id,scope:{models:["*"]},expires_at:0,idempotency_key}` → `{api_key}` |
| 用户态接口 | `GET /api/verdent/home` → `{is_login, user_info, teams[]}`，`teams[0].team_id` 为个人/团队空间 id |
| 业务响应壳 | `{errCode,errMsg,data,details}`，`errCode!==0` 即失败 |
| 鉴权失败响应 | `{"type":"error","error":{"type":"authentication_error","message":"..."}}` |

> 关键判断：`cloud.verdent.ai` 内部虽用 tRPC（`sessions.send` / `sessions.subscribeStream` over `/trpc` + `/trpc-ws`），但 `api.verdent.ai` 本身就是标准 OpenAI 兼容网关，因此本项目直接透传 `/v1/*`，无需实现 tRPC——复杂度大幅下降。

## 目录结构

```
.
├── cmd/
│   ├── server/        # 主网关服务
│   └── login/         # 登录工具（Turnstile → passport → 创建 key）
├── internal/
│   ├── auth/          # 凭证加载 / 原子写回
│   ├── platform/      # passport 登录 / home 查询 / 建键
│   ├── upstream/      # api.verdent.ai 客户端 + SSE 规范化 + 错误分类
│   ├── pool/          # key 池：加权轮换 / 冷却禁用状态机 / 持久化
│   └── server/        # OpenAI 兼容 handler
├── auths/             # 凭证（gitignored）
├── data/              # 池状态（gitignored）
├── config.example.json
├── Dockerfile
└── build.sh
```

## 快速开始

### 1. 构建

```bash
./build.sh                 # vet + test + 构建 ./bin
# 或
go build ./...
```

### 2. 登录并创建 API key

```bash
./bin/verdent-login                    # 默认监听 :17867
# 或
./bin/verdent-login -listen :17867 -keys ./auths/verdent-keys.json -name my-key
```

浏览器打开 `http://127.0.0.1:17867`，输入邮箱密码并完成人机验证（Turnstile 必须人工完成，无法绕过），工具会自动：

1. `POST login.verdent.ai/passport/login` 拿登录态 cookie
2. `GET platform.verdent.ai/api/verdent/home` 校验登录并取 `team_id`
3. `POST platform.verdent.ai/api/verdent/team/api-keys/create` 创建 key
4. 追加写入 `auths/verdent-keys.json`（已有 key 不会丢失）

> 网关运行时会每 15s 热重载凭证文件，登录写入新 key 后**无需重启**，约 15s 内自动可用。

页面会直接显示创建好的明文 key（仅此一次可见）。

### 3. 启动网关

```bash
cp config.example.json config.json
# 至少把 api_key 改成一个随机长串（留空 = 不鉴权）
./bin/verdent-server -config config.json
```

Docker：

```bash
docker build -t verdent2api .
docker run -d -p 7866:7866 \
  -v $PWD/auths:/app/auths -v $PWD/data:/app/data \
  -e V2A_API_KEY=一个随机密钥 \
  verdent2api
```

### 4. 调用

```bash
curl http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $V2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<上游模型名>","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

Base URL 填 `http://127.0.0.1:7866/v1`，可直接配置进 CherryStudio / Cline / chatgpt-oa 等 OpenAI 兼容客户端。

### 5. 查看状态

```bash
curl http://127.0.0.1:7866/status -H "Authorization: Bearer $V2A_API_KEY"
```

输出每个 key 的脱敏标识、状态（healthy/cooldown/disabled）、请求数、成功数、token 计数与最近错误。

## 账号池与错误分类

| 上游表现 | 分类 | 处理 |
|---|---|---|
| HTTP 401/403（authentication_error） | `key_dead` | **禁用**，需人工换 key（`/status` 可见；重新登录或手动改凭证后，网关 15s 内热重载自动纳入，无需重启） |
| HTTP 402 / 正文命中 quota/insufficient/credits | `no_credit` | 冷却 `12h`（`pool.credit_cooldown`） |
| HTTP 429 | `rate_limited` | 冷却 `60s`（`pool.rate_cooldown`） |
| 连续失败次数达阈值 | — | 冷却 `10m`（`pool.err_threshold` / `pool.err_cooldown`） |
| 5xx | `transient` | 不入状态机，直接透传错误 |
| 全池不可用 | — | OpenAI 风格 `503 no_healthy_account` |

成功请求会重置连续错误计数，并累计 prompt/completion token。

## 模型别名

`config.json`：

```jsonc
{
  "model_alias": {
    "fast": "<上游真实模型名>"
  }
}
```

客户端用 `fast` 时会被改写为上游真实模型名再透传，`fast` 也会出现在 `/v1/models` 里。`model_map` 是同语义的旧字段名（只改写请求、不暴露到 models 列表）。

## 配置项

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `:7866` | 监听地址 |
| `api_key` | `""` | 网关侧鉴权 key，空 = 不鉴权（建议设随机长串） |
| `key_file` | `./auths/verdent-keys.json` | API key 凭证文件 |
| `state_file` | `./data/state.json` | 池状态持久化 |
| `upstream.*` | `https://api.verdent.ai` | 上游地址与超时（秒） |
| `pool.*` | 见上表 | 池调参 |
| `scheduler.keepalive_minutes` | `30` | 后台探活间隔（拉 `/v1/models`，只探测不入状态机） |
| `features.strip_reasoning` | `true` | 流式帧里移除 DeepSeek 系 `reasoning_content`（部分客户端不识别） |

环境变量覆盖：`V2A_API_KEY` / `V2A_LISTEN` / `V2A_KEY_FILE` / `V2A_STATE_FILE`。

## 测试

```bash
go test ./...
```

覆盖：SSE 解析与规范化、错误分类、池选号/粘性/冷却/禁用、handler 端到端（mock 上游）、OpenAI 错误格式。
