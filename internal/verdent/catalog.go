package verdent

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// CatalogModel 一个模型目录项（取自桌面版 model-catalog-cache.json 的 model_config）。
type CatalogModel struct {
	Key             string `json:"key"`
	DisplayName     string `json:"display_name,omitempty"`
	Label           string `json:"label,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Description     string `json:"description,omitempty"`
	ContextWindow   int    `json:"context_window_tokens,omitempty"`
	MaxOutputTokens int    `json:"default_max_output_tokens,omitempty"`
	SupportsImages  bool   `json:"supportsImages,omitempty"`
	SupportsThink   bool   `json:"supports_thinking,omitempty"`
	IsLimitFree     bool   `json:"is_limit_free,omitempty"`
}

// IsFree 判断是否限免：目录标记 is_limit_free 或 key 带 -free 后缀。
func (m CatalogModel) IsFree() bool {
	return m.IsLimitFree || IsFreeModel(m.Key)
}

// OpenAIObject 转成 OpenAI /v1/models 项，附 is_free 标记。
func (m CatalogModel) OpenAIObject() map[string]interface{} {
	owned := m.Provider
	if owned == "" {
		owned = "verdent"
	}
	name := m.DisplayName
	if name == "" {
		name = m.Label
	}
	return map[string]interface{}{
		"id":                m.Key,
		"object":            "model",
		"created":           0,
		"owned_by":          owned,
		"display_name":      name,
		"description":       m.Description,
		"context_window":    m.ContextWindow,
		"max_output_tokens": m.MaxOutputTokens,
		"supports_images":   m.SupportsImages,
		"supports_thinking": m.SupportsThink,
		"is_free":           m.IsFree(),
	}
}

// defaultCatalog 是内嵌兜底目录（桌面版缓存不可用时使用，保证 headless 部署可列模型）。
// 逆向自 Verdent 前端与参考实现，限免模型以 -free 后缀标识。
//
//go:embed catalog_default.json
var defaultCatalogRaw []byte

// Catalog 模型目录，支持从桌面版缓存热加载 + 内嵌兜底。
type Catalog struct {
	mu     sync.RWMutex
	path   string
	models []CatalogModel
}

// catalogFile 桌面版缓存的默认路径。
func catalogFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".verdent", "model-catalog-cache.json")
}

// NewCatalog 构造目录：path 为空时用桌面版默认路径，读不到则回落内嵌目录。
func NewCatalog(path string) *Catalog {
	if strings.TrimSpace(path) == "" {
		path = catalogFile()
	}
	c := &Catalog{path: path}
	c.Reload()
	return c
}

// Reload 重读目录来源；桌面版缓存不可用时保留内嵌兜底。
func (c *Catalog) Reload() {
	models := loadCatalogFile(c.path)
	if len(models) == 0 {
		models = parseCatalog(defaultCatalogRaw)
	}
	c.mu.Lock()
	c.models = models
	c.mu.Unlock()
}

// Models 返回目录副本（稳定按 key 排序）。
func (c *Catalog) Models() []CatalogModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CatalogModel, len(c.models))
	copy(out, c.models)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// FreeModels 只返回限免模型。
func (c *Catalog) FreeModels() []CatalogModel {
	out := make([]CatalogModel, 0)
	for _, m := range c.Models() {
		if m.IsFree() {
			out = append(out, m)
		}
	}
	return out
}

// Find 按 key 查一个模型。
func (c *Catalog) Find(key string) (CatalogModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.models {
		if m.Key == key {
			return m, true
		}
	}
	return CatalogModel{}, false
}

// loadCatalogFile 解析桌面版缓存文件（data.model_config）。
func loadCatalogFile(path string) []CatalogModel {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseCatalog(b)
}

// parseCatalog 兼容两种形态：桌面缓存 {data:{model_config:[...]}} 或裸数组 [...]。
func parseCatalog(b []byte) []CatalogModel {
	if len(b) == 0 {
		return nil
	}
	var wrapped struct {
		Data struct {
			ModelConfig []CatalogModel `json:"model_config"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &wrapped) == nil && len(wrapped.Data.ModelConfig) > 0 {
		return filterKeyed(wrapped.Data.ModelConfig)
	}
	var arr []CatalogModel
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 {
		return filterKeyed(arr)
	}
	return nil
}

func filterKeyed(in []CatalogModel) []CatalogModel {
	out := in[:0]
	for _, m := range in {
		if strings.TrimSpace(m.Key) != "" {
			out = append(out, m)
		}
	}
	return out
}
