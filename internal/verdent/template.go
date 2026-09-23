// template.json 是桌面版抓包得到的请求模板。
//
// 关键：body.system 字段被网关做指纹校验，必须原样携带桌面 App 自己的
// agent prompt——任何替换都会被打进严格的 20004 限流车道。因此客户端传入的
// system 只能折叠进 messages，绝不能覆盖 body.system。
//
// App 更新其 prompt 时，用 capture 流程重新抓取 template.json 并替换本文件。
package verdent

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed template.json
var templateRaw []byte

var (
	templateOnce sync.Once
	templateMap  map[string]json.RawMessage
)

// template 返回模板字段 map（惰性解析一次）。
func template() map[string]json.RawMessage {
	templateOnce.Do(func() {
		templateMap = make(map[string]json.RawMessage)
		_ = json.Unmarshal(templateRaw, &templateMap)
	})
	// 返回浅拷贝，避免调用方污染缓存。
	out := make(map[string]json.RawMessage, len(templateMap))
	for k, v := range templateMap {
		out[k] = v
	}
	return out
}
