package verdent

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// StreamEvent 是从上游 hybrid-stream 解析出的一个语义事件。
// 上游是 Anthropic 风格：message_start / content_block_start /
// content_block_delta(text_delta|thinking_delta|input_json_delta) /
// message_delta / message_stop。
type StreamEvent struct {
	Type string
	// 文本增量（text_delta）。
	Text string
	// 思维链增量（thinking_delta）。
	Reasoning string
	// 工具调用相关。
	ToolIndex int
	ToolID    string
	ToolName  string
	ToolArgs  string // input_json_delta 的 partial_json 增量
	// usage。
	InputTokens  int
	OutputTokens int
	// 停止原因（message_delta 里的 stop_reason）。
	StopReason string
	// 错误信息（stream_error/error）。
	ErrMsg string
	// Done 表示流结束（[DONE] 或 message_stop）。
	Done bool
}

// SSEDecoder 逐帧解析上游 SSE。
type SSEDecoder struct {
	r *bufio.Reader
}

// NewSSEDecoder 包装上游 body。
func NewSSEDecoder(r io.Reader) *SSEDecoder {
	return &SSEDecoder{r: bufio.NewReaderSize(r, 64<<10)}
}

// rawFrame 累积一个以空行分隔的 SSE 事件块。
type rawFrame struct {
	event string
	data  string
}

// nextFrame 读取下一个完整帧（event: / data: 直到空行）。
func (d *SSEDecoder) nextFrame() (*rawFrame, error) {
	var fr rawFrame
	got := false
	for {
		line, err := d.r.ReadString('\n')
		if len(line) > 0 {
			got = true
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if fr.event != "" || fr.data != "" {
					return &fr, nil
				}
				// 纯空行且无累积，继续。
			case strings.HasPrefix(trimmed, "event:"):
				fr.event = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				seg := strings.TrimPrefix(trimmed[len("data:"):], " ")
				if fr.data == "" {
					fr.data = seg
				} else {
					fr.data += seg
				}
			case strings.HasPrefix(trimmed, ":"):
				// 注释/心跳，忽略。
			}
		}
		if err != nil {
			if got && (fr.event != "" || fr.data != "") {
				return &fr, nil
			}
			return nil, err
		}
	}
}

// Next 返回下一个语义事件；流结束返回 (nil, io.EOF)。
func (d *SSEDecoder) Next() (*StreamEvent, error) {
	for {
		fr, err := d.nextFrame()
		if err != nil {
			return nil, err
		}
		if fr.data == "[DONE]" {
			return &StreamEvent{Type: "done", Done: true}, nil
		}
		if fr.data == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(fr.data), &obj) != nil {
			continue
		}
		ev := decodeEvent(fr.event, obj)
		if ev == nil {
			continue
		}
		return ev, nil
	}
}

// decodeEvent 把一帧 JSON 转成 StreamEvent；心跳返回 nil。
func decodeEvent(eventName string, obj map[string]json.RawMessage) *StreamEvent {
	t := jsonString(obj["type"])
	if t == "" {
		t = eventName
	}
	switch t {
	case "heartbeat", "ping":
		return nil
	case "stream_error", "error":
		msg := ""
		if e, ok := obj["error"]; ok {
			var em map[string]json.RawMessage
			if json.Unmarshal(e, &em) == nil {
				msg = jsonString(em["message"])
			}
		}
		if msg == "" {
			msg = "upstream stream error"
		}
		return &StreamEvent{Type: "error", ErrMsg: msg, Done: true}
	case "message_start":
		ev := &StreamEvent{Type: t}
		if m, ok := obj["message"]; ok {
			var mm map[string]json.RawMessage
			if json.Unmarshal(m, &mm) == nil {
				if u, ok := mm["usage"]; ok {
					in, _ := usageTokens(u)
					ev.InputTokens = in
				}
			}
		}
		return ev
	case "content_block_start":
		ev := &StreamEvent{Type: t}
		blk := obj["content_block"]
		if blk == nil {
			blk = obj["content"]
		}
		if blk != nil {
			var bm map[string]json.RawMessage
			if json.Unmarshal(blk, &bm) == nil && jsonString(bm["type"]) == "tool_use" {
				ev.Type = "tool_start"
				ev.ToolIndex = jsonInt(obj["index"])
				ev.ToolID = jsonString(bm["id"])
				ev.ToolName = jsonString(bm["name"])
			}
		}
		return ev
	case "content_block_delta":
		var d map[string]json.RawMessage
		if json.Unmarshal(obj["delta"], &d) != nil {
			return &StreamEvent{Type: t}
		}
		switch jsonString(d["type"]) {
		case "text_delta":
			return &StreamEvent{Type: "text", Text: jsonString(d["text"])}
		case "thinking_delta":
			return &StreamEvent{Type: "reasoning", Reasoning: jsonString(d["thinking"])}
		case "input_json_delta", "arguments_delta":
			args := jsonString(d["partial_json"])
			if args == "" {
				args = jsonString(d["arguments"])
			}
			return &StreamEvent{Type: "tool_args", ToolIndex: jsonInt(obj["index"]), ToolArgs: args}
		}
		return &StreamEvent{Type: t}
	case "message_delta":
		ev := &StreamEvent{Type: t}
		if u, ok := obj["usage"]; ok {
			_, out := usageTokens(u)
			ev.OutputTokens = out
		}
		if d, ok := obj["delta"]; ok {
			var dm map[string]json.RawMessage
			if json.Unmarshal(d, &dm) == nil {
				ev.StopReason = jsonString(dm["stop_reason"])
			}
		}
		return ev
	case "message_stop":
		return &StreamEvent{Type: t, Done: true}
	}
	return &StreamEvent{Type: t}
}

func usageTokens(raw json.RawMessage) (in, out int) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, 0
	}
	return jsonInt(m["input_tokens"]), jsonInt(m["output_tokens"])
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func jsonInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	return 0
}
