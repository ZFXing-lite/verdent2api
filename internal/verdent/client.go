package verdent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"verdent2api/internal/crypto"
)

// 上游常量（逆向自桌面版 HttpAiProvider）。
const (
	ProxyBase   = "https://llm-proxy.verdent.ai"
	streamPath  = "/llm/stream"
	betaHeader  = "hybrid-stream@20250919"
	versionCode = "2.15.1"
	UserAgent   = "Verdent/2.15.1"
	// deviceID 不再全局共享：每账号持久化唯一值（见 Store.EnsureDeviceID），
	// 避免多账号同设备ID 被风控关联。桌面版从注册表 MachineGuid 读取。
)

// Client llm-proxy 客户端。
type Client struct {
	Base string
	HTTP tls_client.HttpClient
}

// NewClient 构造客户端。timeout 为整体超时。
// HTTP 走 tls-client Chrome 完整指纹（TLS JA3 + HTTP/2 + 头顺序），传输层不再暴露 Go 特征。
func NewClient(base string, timeout, headerTimeout, idleTimeout time.Duration) *Client {
	if strings.TrimSpace(base) == "" {
		base = ProxyBase
	}
	base = strings.TrimRight(base, "/")
	return &Client{Base: base, HTTP: newFingerprintClient(orDur(timeout, 180*time.Second))}
}

func orDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// OAIMessage 客户端传入的 OpenAI 消息（只取需要的字段）。
type OAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []OAIToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Model      string          `json:"model,omitempty"`
}

// OAIToolCall OpenAI 工具调用。
type OAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// BuildParams 构造上游 body 的入参。
type BuildParams struct {
	Model       string
	Messages    []OAIMessage
	System      string // 客户端显式 system（会被折进消息，不覆盖 body.system）
	ConvID      string // 会话粘性键；非空则复用 conv_id，避免每次请求都新建会话
	MaxTokens   int
	Temperature *float64
	Stream      bool
	Tools       json.RawMessage
	ToolChoice  json.RawMessage
}

// headers 返回桌面版 HttpAiProvider 发往 /llm/stream 的精确头集合。
// deviceID 由调用方按账号传入，确保每账号唯一设备指纹。
// 头发送顺序由 transport.go 的 streamHeaderOrder 经 tls-client 固定，不依赖此 map 迭代序。
func headers(token, deviceID string) map[string]string {
	if strings.TrimSpace(deviceID) == "" {
		// 防御：未提供设备ID时按 token 派生，至少不跨账号共享同一指纹。
		h := sha256.Sum256([]byte(token))
		deviceID = hex.EncodeToString(h[:16])
	}
	return map[string]string{
		"Authorization":      "Bearer " + token,
		"Content-Type":       "application/json",
		"Accept":             "text/event-stream",
		"Accept-Language":    "en-US,en;q=0.9",
		"User-Agent":         UserAgent,
		"verdent-proxy-beta": betaHeader,
		"X-Device-Id":        deviceID,
		"X-Version-Code":     versionCode,
		"X-Team-ID":          "0",
		"X-Device-Type":      "pc",
		"X-OS-Type":          "windows",
	}
}

// IsFreeModel 按命名规律判断限免模型（-free 后缀）。
func IsFreeModel(model string) bool { return strings.HasSuffix(model, "-free") }

// textOfContent 把 OpenAI content（字符串或多模态数组）抽成纯文本片段。
func textOfContent(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var parts []map[string]interface{}
	if json.Unmarshal(raw, &parts) == nil {
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if t, ok := p["text"].(string); ok && t != "" {
				out = append(out, t)
				continue
			}
			if typ, ok := p["type"].(string); ok && (typ == "image_url" || typ == "image") {
				out = append(out, "[image omitted]")
			}
		}
		return out
	}
	return nil
}

// contentString 把 content 归一成字符串（数组则 JSON 化）。
func contentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// appMsg 构造 App 形态消息：content 是文本块数组，首块为 <timestamp>，
// 最后一条消息的最后一块带 cache_control。
func appMsg(role string, texts []string, model string, last bool) map[string]interface{} {
	cst := time.FixedZone("CST", 8*3600) // 固定 +0800，避免暴露服务器实际时区
	now := time.Now().In(cst)
	off := now.Format("-0700")
	ts := "<timestamp>" + now.Format("Mon Jan 02 2006 15:04:05 GMT") + off + "</timestamp>\n"
	blocks := []map[string]interface{}{{"type": "text", "text": ts}}
	for _, t := range texts {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
	}
	if last && len(blocks) > 0 {
		blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}
	}
	out := map[string]interface{}{"role": role, "content": blocks}
	if role == "assistant" && model != "" {
		out["model"] = model
	}
	return out
}

// BuildBody 构造 /llm/stream 的完整请求体（messages/tools 加密）。
func (c *Client) BuildBody(p BuildParams) (map[string]json.RawMessage, error) {
	tpl := template()

	// 1) 收集所有 system 文本（客户端显式 system + 消息里的 system 角色）。
	var sysParts []string
	if strings.TrimSpace(p.System) != "" {
		sysParts = append(sysParts, p.System)
	}
	var convo []OAIMessage
	for _, m := range p.Messages {
		if m.Role == "system" {
			if s := contentString(m.Content); s != "" {
				sysParts = append(sysParts, s)
			}
			continue
		}
		convo = append(convo, m)
	}

	// 2) tool_call id -> name 映射（把工具调用/结果渲染成文本，App 无 tool 角色）。
	id2name := map[string]string{}
	for _, m := range convo {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				id2name[tc.ID] = tc.Function.Name
			}
		}
	}

	// 3) 归一每条消息为 (role, 文本)。
	type norm struct {
		role  string
		texts []string
		model string
	}
	var normed []norm
	for _, m := range convo {
		switch m.Role {
		case "assistant":
			texts := textOfContent(m.Content)
			base := strings.Join(texts, "\n")
			var buf []string
			if base != "" {
				buf = append(buf, base)
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				buf = append(buf, fmt.Sprintf("[tool_call %s] %s", tc.Function.Name, args))
			}
			joined := strings.Join(buf, "\n")
			normed = append(normed, norm{role: "assistant", texts: []string{joined}, model: m.Model})
		case "tool":
			nm := id2name[m.ToolCallID]
			head := "[tool_result]\n"
			if nm != "" {
				head = "[tool_result " + nm + "]\n"
			}
			normed = append(normed, norm{role: "user", texts: []string{head + contentString(m.Content)}})
		default:
			normed = append(normed, norm{role: m.Role, texts: textOfContent(m.Content)})
		}
	}

	// 4) system 折叠进首条 user 消息（或作为新的首条 user 插入）。
	if len(sysParts) > 0 {
		block := "<system>\n" + strings.Join(sysParts, "\n\n") + "\n</system>"
		if len(normed) > 0 && normed[0].role == "user" {
			joined := strings.Join(normed[0].texts, "\n")
			normed[0].texts = []string{block + "\n\n" + joined}
		} else {
			normed = append([]norm{{role: "user", texts: []string{block}}}, normed...)
		}
	}
	if len(normed) == 0 {
		normed = []norm{{role: "user", texts: []string{"Hello"}}}
	}

	// 5) 转 App 形态并加密。
	outMsgs := make([]map[string]interface{}, 0, len(normed))
	for i, n := range normed {
		mdl := n.model
		if n.role == "assistant" && mdl == "" {
			mdl = p.Model
		}
		outMsgs = append(outMsgs, appMsg(n.role, n.texts, mdl, i == len(normed)-1))
	}
	encMsgs, err := crypto.EncryptObj(outMsgs)
	if err != nil {
		return nil, err
	}

	// 6) 组装 body（以模板为底，覆盖动态字段）。
	body := tpl
	maxTok := p.MaxTokens
	if maxTok <= 0 {
		maxTok = 64000
		if v, ok := tpl["max_tokens"]; ok {
			var mt int
			if json.Unmarshal(v, &mt) == nil && mt > 0 {
				maxTok = mt
			}
		}
	}
	body["model"] = jraw(p.Model)
	body["session_id"] = jraw("session_" + uuidv4())
	conv := p.ConvID
	if conv == "" {
		conv = uuidv4()
	}
	body["conv_id"] = jraw("conv_" + conv)
	body["react_id"] = jraw("model_agent_" + uuidv4())
	body["react_type"] = jraw("Main Agent")
	body["stream"] = jraw(p.Stream)
	body["max_tokens"] = jraw(maxTok)
	body["messages"] = jraw(encMsgs)
	delete(body, "sub_type")

	// env: 保留模板其余字段，覆盖 today_date。
	envMap := map[string]interface{}{}
	if v, ok := tpl["env"]; ok {
		_ = json.Unmarshal(v, &envMap)
	}
	envMap["today_date"] = time.Now().Format("2006-01-02")
	body["env"] = jraw(envMap)

	if len(p.Tools) > 0 {
		if conv := convertTools(p.Tools); conv != nil {
			enc, terr := crypto.EncryptObj(conv)
			if terr != nil {
				return nil, terr
			}
			body["tools"] = jraw(enc)
			body["tool_choice"] = jraw(convertToolChoice(p.ToolChoice))
		}
	}
	if p.Temperature != nil {
		body["temperature"] = jraw(*p.Temperature)
	}
	return body, nil
}

// Stream POST /llm/stream，返回上游响应（调用方负责关闭 Body）。
// deviceID 按账号传入，用于 X-Device-Id 头，确保每账号唯一设备指纹。
// 响应为 *fhttp.Response（tls-client），Body 是 io.ReadCloser，与翻译层兼容。
func (c *Client) Stream(ctx context.Context, token, deviceID string, body map[string]json.RawMessage) (*fhttp.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, c.Base+streamPath, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range headers(token, deviceID) {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return resp, nil
}

// UpstreamError 上游非 2xx。
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "..."
	}
	return fmt.Sprintf("upstream %d: %s", e.Status, b)
}

// jraw 把值编码成 json.RawMessage（构造 body 用）。
func jraw(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
