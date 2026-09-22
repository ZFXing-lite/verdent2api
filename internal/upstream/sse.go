package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Chunk 标准 OpenAI chat.completion.chunk。
type Chunk struct {
	ID                string   `json:"id,omitempty"`
	Object            string   `json:"object,omitempty"`
	Created           int64    `json:"created,omitempty"`
	Model             string   `json:"model,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
}

// Choice chunk 内的 choice。
type Choice struct {
	Index        int             `json:"index"`
	Delta        Delta           `json:"delta,omitempty"`
	Message      *Delta          `json:"message,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// Delta 增量内容。ReasoningContent 为 DeepSeek 系思维链字段。
type Delta struct {
	Role             string        `json:"role,omitempty"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
	ToolCalls        []interface{} `json:"tool_calls,omitempty"`
	FunctionCall     interface{}   `json:"function_call,omitempty"`
	Refusal          string        `json:"refusal,omitempty"`
}

// Usage token 计数。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// SSEReader 逐帧解析上游 SSE 流。
type SSEReader struct {
	r *bufio.Reader
}

// NewSSEReader 包装上游 body。
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next 返回下一帧的原始 data 负载；遇到 [DONE] 返回 (nil, io.EOF)。
// 跳过注释行（: ping）与空行。
func (s *SSEReader) Next() ([]byte, error) {
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if len(line) == 0 {
				return nil, err
			}
			// 最后一行无换行符也要处理。
			line = strings.TrimRight(line, "\r\n")
			if trimmed := strings.TrimSpace(line); trimmed == "" {
				return nil, io.EOF
			}
			return s.parseLine(line)
		}
		line = strings.TrimRight(line, "\r\n")
		if trimmed := strings.TrimSpace(line); trimmed == "" {
			continue // 事件分隔
		}
		if strings.HasPrefix(line, ":") {
			continue // 注释/心跳
		}
		return s.parseLine(line)
	}
}

func (s *SSEReader) parseLine(line string) ([]byte, error) {
	if strings.HasPrefix(line, "data:") {
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			return nil, io.EOF
		}
		return []byte(payload), nil
	}
	// event:/id:/retry: 等字段暂不处理（OpenAI 协议不用）。
	return nil, nil
}

// ParseChunk 解析一帧 data 为 Chunk；非法 JSON 时返回错误。
func ParseChunk(data []byte) (*Chunk, error) {
	var c Chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// isEmptyDelta 判断 delta 是否全空（包含不可比较字段，不能直接 ==）。
func isEmptyDelta(d Delta) bool {
	return d.Role == "" && d.Content == "" && d.ReasoningContent == "" &&
		d.Reasoning == "" && d.Refusal == "" && len(d.ToolCalls) == 0 &&
		d.FunctionCall == nil
}

// Normalize 把上游 chunk 规范成标准 OpenAI chat.completion.chunk JSON。
// model 为回填的模型名（上游缺失时用），idSeed 用于生成稳定 id。
func Normalize(c *Chunk, model, idPrefix string, created int64) []byte {
	if c.Object == "" {
		c.Object = "chat.completion.chunk"
	}
	if c.ID == "" {
		c.ID = idPrefix
	}
	if c.Created == 0 {
		c.Created = created
	}
	if c.Model == "" {
		c.Model = model
	}
	for i := range c.Choices {
		ch := &c.Choices[i]
		// 兼容上游把首帧放在 message 而非 delta 的情况。
		if ch.Message != nil && isEmptyDelta(ch.Delta) {
			ch.Delta = *ch.Message
			ch.Message = nil
		}
		// 统一思维链字段到 reasoning_content。
		if ch.Delta.Reasoning != "" && ch.Delta.ReasoningContent == "" {
			ch.Delta.ReasoningContent = ch.Delta.Reasoning
			ch.Delta.Reasoning = ""
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return []byte(fmt.Sprintf("{\"error\":\"normalize failed: %s\"}", err))
	}
	return b
}

// Frame 把已规范化的 JSON 负载包成 SSE 帧。
func Frame(payload []byte) []byte {
	return []byte("data: " + string(payload) + "\n\n")
}

// DoneFrame 结束帧。
func DoneFrame() []byte {
	return []byte("data: [DONE]\n\n")
}

// ErrorFrame 把错误信息包成 OpenAI 风格的 SSE 错误帧（客户端可识别）。
func ErrorFrame(msg string) []byte {
	payload, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "upstream_error",
			"code":    nil,
		},
	})
	return Frame(payload)
}

// NewChunkID 生成一个 chunk id 前缀（与 OpenAI 风格一致）。
func NewChunkID() string {
	return "chatcmpl-" + strings.ReplaceAll(time.Now().Format("20060102150405.000000"), ".", "")
}
