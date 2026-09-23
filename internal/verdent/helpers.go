package verdent

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
)

// urlEncode 转义 URL 查询参数值。
func urlEncode(s string) string { return url.QueryEscape(s) }

// uuidv4 生成随机 UUIDv4 字符串（用于 session_id/conv_id 等）。
func uuidv4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// convertTools 把 OpenAI tools 转成 App 形态 [{name,description,input_schema}]。
// 输入是原始 JSON（客户端请求里的 tools 数组）；无有效项时返回 nil。
func convertTools(raw json.RawMessage) []map[string]interface{} {
	var arr []map[string]json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(arr))
	for _, t := range arr {
		// 支持 {type:"function",function:{...}} 与直接 {name,...} 两种形态。
		fn := t
		if typ, ok := t["type"]; ok {
			var ts string
			if json.Unmarshal(typ, &ts) == nil && ts == "function" {
				if f, ok := t["function"]; ok {
					var fm map[string]json.RawMessage
					if json.Unmarshal(f, &fm) == nil {
						fn = fm
					}
				}
			}
		}
		var name string
		if n, ok := fn["name"]; ok {
			_ = json.Unmarshal(n, &name)
		}
		if name == "" {
			continue
		}
		var desc string
		if d, ok := fn["description"]; ok {
			_ = json.Unmarshal(d, &desc)
		}
		schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		if p, ok := fn["parameters"]; ok {
			var pm map[string]interface{}
			if json.Unmarshal(p, &pm) == nil && len(pm) > 0 {
				schema = pm
			}
		}
		out = append(out, map[string]interface{}{
			"name":         name,
			"description":  desc,
			"input_schema": schema,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// convertToolChoice 把 OpenAI tool_choice 转成 App 形态 dict。
// 缺省 {type:"auto"}；"required"→{type:"any"}；指定函数→{type:"tool",name}。
func convertToolChoice(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{"type": "auto"}
	}
	// 字符串形态："auto" / "none" / "required" / "any"
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "required":
			return map[string]interface{}{"type": "any"}
		case "any":
			return map[string]interface{}{"type": "any"}
		default:
			return map[string]interface{}{"type": "auto"}
		}
	}
	// 对象形态。
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		var typ string
		if t, ok := m["type"]; ok {
			_ = json.Unmarshal(t, &typ)
		}
		if typ == "function" {
			if f, ok := m["function"]; ok {
				var fm map[string]json.RawMessage
				if json.Unmarshal(f, &fm) == nil {
					var name string
					if n, ok := fm["name"]; ok {
						_ = json.Unmarshal(n, &name)
					}
					if name != "" {
						return map[string]interface{}{"type": "tool", "name": name}
					}
				}
			}
		}
		if typ == "any" {
			return map[string]interface{}{"type": "any"}
		}
	}
	return map[string]interface{}{"type": "auto"}
}
