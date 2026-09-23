package verdent

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Usage 汇总的 token 计数。
type Usage struct {
	In  int
	Out int
}

// Translator 把上游 hybrid-stream 翻译成 OpenAI 格式（流式 chunk 或聚合 JSON）。
type Translator struct {
	dec     *SSEDecoder
	model   string
	id      string
	created int64
}

// NewTranslator 包装上游响应 body。
func NewTranslator(r io.Reader, model string) *Translator {
	return &Translator{
		dec:     NewSSEDecoder(r),
		model:   model,
		id:      "chatcmpl-" + uuidv4(),
		created: time.Now().Unix(),
	}
}

// toolAcc 累积一个工具调用。
type toolAcc struct {
	id   string
	name string
	args string
}

// StreamTo 逐事件翻译成 OpenAI SSE chunk，通过 emit 回调发出（已含 data: 前缀与结尾）。
// 返回累计 usage。函数负责发送结尾的 tool_calls chunk、finish chunk 与 [DONE]。
func (t *Translator) StreamTo(emit func([]byte)) (Usage, error) {
	var usage Usage
	var reasoning []byte
	tools := map[int]*toolAcc{}
	var toolOrder []int
	finish := "stop"
	roleSent := false

	for {
		ev, err := t.dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return usage, err
		}
		switch ev.Type {
		case "error":
			return usage, fmt.Errorf("%s", ev.ErrMsg)
		case "message_start":
			if ev.InputTokens > 0 {
				usage.In = ev.InputTokens
			}
		case "text":
			if ev.Text == "" {
				continue
			}
			delta := map[string]interface{}{"content": ev.Text}
			if !roleSent {
				delta["role"] = "assistant"
				roleSent = true
			}
			emit(t.chunk(delta, nil))
		case "reasoning":
			reasoning = append(reasoning, ev.Reasoning...)
		case "tool_start":
			if _, ok := tools[ev.ToolIndex]; !ok {
				toolOrder = append(toolOrder, ev.ToolIndex)
			}
			tools[ev.ToolIndex] = &toolAcc{id: ev.ToolID, name: ev.ToolName}
			if tools[ev.ToolIndex].id == "" {
				tools[ev.ToolIndex].id = "call_" + uuidv4()[:12]
			}
		case "tool_args":
			ta := tools[ev.ToolIndex]
			if ta == nil {
				ta = &toolAcc{id: "call_" + uuidv4()[:12]}
				tools[ev.ToolIndex] = ta
				toolOrder = append(toolOrder, ev.ToolIndex)
			}
			ta.args += ev.ToolArgs
		case "message_delta":
			if ev.OutputTokens > 0 {
				usage.Out = ev.OutputTokens
			}
			if ev.StopReason == "tool_use" {
				finish = "tool_calls"
			}
		}
		if ev.Done {
			break
		}
	}

	// 思维链作为独立 chunk 发出（部分客户端识别 reasoning_content）。
	if len(reasoning) > 0 {
		emit(t.chunk(map[string]interface{}{"reasoning_content": string(reasoning)}, nil))
	}
	// 工具调用汇总 chunk。
	if len(toolOrder) > 0 {
		calls := make([]map[string]interface{}, 0, len(toolOrder))
		for i, idx := range toolOrder {
			ta := tools[idx]
			calls = append(calls, map[string]interface{}{
				"index": i,
				"id":    ta.id,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      ta.name,
					"arguments": ta.args,
				},
			})
		}
		emit(t.chunk(map[string]interface{}{"tool_calls": calls}, nil))
		finish = "tool_calls"
	}
	// finish chunk + [DONE]。
	emit(t.chunk(map[string]interface{}{}, &finish))
	emit(SSEDoneFrame())
	return usage, nil
}

// Aggregate 消费整个流，聚合成一个 OpenAI chat.completion JSON。
func (t *Translator) Aggregate() ([]byte, Usage, error) {
	var usage Usage
	var content []byte
	var reasoning []byte
	tools := map[int]*toolAcc{}
	var toolOrder []int
	finish := "stop"

	for {
		ev, err := t.dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, usage, err
		}
		switch ev.Type {
		case "error":
			return nil, usage, fmt.Errorf("%s", ev.ErrMsg)
		case "message_start":
			if ev.InputTokens > 0 {
				usage.In = ev.InputTokens
			}
		case "text":
			content = append(content, ev.Text...)
		case "reasoning":
			reasoning = append(reasoning, ev.Reasoning...)
		case "tool_start":
			if _, ok := tools[ev.ToolIndex]; !ok {
				toolOrder = append(toolOrder, ev.ToolIndex)
			}
			ta := &toolAcc{id: ev.ToolID, name: ev.ToolName}
			if ta.id == "" {
				ta.id = "call_" + uuidv4()[:12]
			}
			tools[ev.ToolIndex] = ta
		case "tool_args":
			ta := tools[ev.ToolIndex]
			if ta == nil {
				ta = &toolAcc{id: "call_" + uuidv4()[:12]}
				tools[ev.ToolIndex] = ta
				toolOrder = append(toolOrder, ev.ToolIndex)
			}
			ta.args += ev.ToolArgs
		case "message_delta":
			if ev.OutputTokens > 0 {
				usage.Out = ev.OutputTokens
			}
			if ev.StopReason == "tool_use" {
				finish = "tool_calls"
			}
		}
		if ev.Done {
			break
		}
	}

	msg := map[string]interface{}{"role": "assistant"}
	if len(content) > 0 {
		msg["content"] = string(content)
	} else {
		msg["content"] = nil
	}
	if len(reasoning) > 0 {
		msg["reasoning_content"] = string(reasoning)
	}
	if len(toolOrder) > 0 {
		calls := make([]map[string]interface{}, 0, len(toolOrder))
		for _, idx := range toolOrder {
			ta := tools[idx]
			calls = append(calls, map[string]interface{}{
				"id":   ta.id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      ta.name,
					"arguments": ta.args,
				},
			})
		}
		msg["tool_calls"] = calls
		finish = "tool_calls"
	}

	out := map[string]interface{}{
		"id":      t.id,
		"object":  "chat.completion",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"finish_reason": finish,
			"message":       msg,
		}},
		"usage": map[string]interface{}{
			"prompt_tokens":     usage.In,
			"completion_tokens": usage.Out,
			"total_tokens":      usage.In + usage.Out,
		},
	}
	b, err := json.Marshal(out)
	return b, usage, err
}

// chunk 构造一个 OpenAI chat.completion.chunk 的 SSE 帧。
func (t *Translator) chunk(delta map[string]interface{}, finish *string) []byte {
	obj := map[string]interface{}{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	b, _ := json.Marshal(obj)
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// SSEDoneFrame 结束帧。
func SSEDoneFrame() []byte { return []byte("data: [DONE]\n\n") }

// SSEErrorFrame OpenAI 风格错误帧。
func SSEErrorFrame(msg string) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": msg, "type": "upstream_error"},
	})
	return append(append([]byte("data: "), b...), '\n', '\n')
}
