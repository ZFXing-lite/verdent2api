// Package auth 加载并持久化 api.verdent.ai 的 API key 凭证。
//
// 凭证文件是一个 JSON 数组，每项是一个上游 key 条目：
//
//	[{"api_key":"sk-...","label":"main","created_by":"email"}]
//
// 池的运行时状态（冷却 / 计数 / 余额）不落在这里，见 internal/pool 的 state.json。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Entry 一条上游 API key 凭证。
type Entry struct {
	APIKey    string `json:"api_key"`
	Label     string `json:"label,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// Store 凭证存储，线程安全。
type Store struct {
	mu      sync.RWMutex
	path    string
	entries []Entry
}

// New 从 path 加载凭证；文件不存在时返回空 store（不报错）。
func New(path string) (*Store, error) {
	s := &Store{path: path}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	// 容错：文件为空时当作空库。
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse key file %s: %w", path, err)
	}
	s.entries = entries
	return s, nil
}

// Reload 重新从磁盘读取凭证文件并替换内存快照。
// 登录工具作为独立进程在服务运行期间追加 key 后，
// 服务侧调用本方法 + pool.Refresh 即可免重启纳入账号池。
func (s *Store) Reload() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.entries = nil
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("reload key file %s: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		s.mu.Lock()
		s.entries = nil
		s.mu.Unlock()
		return nil
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("parse key file %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
	return nil
}

// All 返回全部条目副本。
func (s *Store) All() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Keys 返回去重后的 api_key 列表，按字母序稳定。
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{}, len(s.entries))
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		k := strings.TrimSpace(e.APIKey)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Add 追加一条凭证并原子写回；重复的 key 会被忽略。
func (s *Store) Add(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.TrimSpace(e.APIKey)
	if k == "" {
		return fmt.Errorf("empty api_key")
	}
	for _, ex := range s.entries {
		if ex.APIKey == k {
			return fmt.Errorf("api_key already exists")
		}
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = nowUnix()
	}
	s.entries = append(s.entries, e)
	return s.saveLocked()
}

// Remove 删除指定 api_key 并写回。
func (s *Store) Remove(apiKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.entries[:0]
	removed := false
	for _, ex := range s.entries {
		if ex.APIKey == apiKey {
			removed = true
			continue
		}
		next = append(next, ex)
	}
	if !removed {
		return fmt.Errorf("api_key not found")
	}
	s.entries = next
	return s.saveLocked()
}

// saveLocked 原子写回（调用方已持锁）。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir for key file: %w", err)
	}
	b, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write tmp key file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename key file: %w", err)
	}
	return nil
}
