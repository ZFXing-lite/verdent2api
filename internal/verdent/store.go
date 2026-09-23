// Package verdent 封装 Verdent 桌面端协议：PKCE 登录、账号 token 存储、
// llm-proxy 加密信封请求、hybrid-stream SSE 翻译、模型目录。
//
// 与旧版（api.verdent.ai + team API key）的本质区别：
// 本包走桌面端真实链路 llm-proxy.verdent.ai/llm/stream，凭证是 PKCE
// 登录换来的 Bearer token（可 refresh），因此才能命中 Free mode 限免模型。
package verdent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Account 一个 Verdent 登录账号的凭证。
type Account struct {
	UserID       string `json:"user_id"`
	Label        string `json:"label,omitempty"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpireAtMS 是 access token 到期的 Unix 毫秒；0 表示未知（不主动判过期）。
	ExpireAtMS   int64 `json:"expire_at_ms,omitempty"`
	ObtainedAtMS int64 `json:"obtained_at_ms,omitempty"`
}

// Valid 判断 token 是否仍可用（留 60s 安全边界）。
func (a *Account) Valid() bool {
	if a == nil || strings.TrimSpace(a.Token) == "" {
		return false
	}
	if a.ExpireAtMS == 0 {
		return true
	}
	return time.Now().UnixMilli() < a.ExpireAtMS-60_000
}

// Store 账号存储，线程安全，原子落盘。
type Store struct {
	mu       sync.RWMutex
	path     string
	accounts []Account
}

// NewStore 从 path 加载账号；文件不存在返回空 store。
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("read auth file %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	// 兼容两种形态：账号数组，或单账号对象。
	var arr []Account
	if err := json.Unmarshal(b, &arr); err == nil {
		s.accounts = arr
		return s, nil
	}
	var one Account
	if err := json.Unmarshal(b, &one); err != nil {
		return nil, fmt.Errorf("parse auth file %s: %w", path, err)
	}
	if one.Token != "" {
		s.accounts = []Account{one}
	}
	return s, nil
}

// Reload 重新从磁盘读取账号。登录工具写入后，网关调用本方法免重启纳入新账号。
func (s *Store) Reload() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.accounts = nil
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("reload auth file %s: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		s.mu.Lock()
		s.accounts = nil
		s.mu.Unlock()
		return nil
	}
	var arr []Account
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("parse auth file %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.accounts = arr
	s.mu.Unlock()
	return nil
}

// All 返回账号副本。
func (s *Store) All() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, len(s.accounts))
	copy(out, s.accounts)
	return out
}

// IDs 返回稳定的账号标识列表（user_id 优先，退回 token 指纹）。
func (s *Store) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.accounts))
	for i := range s.accounts {
		out = append(out, accountID(&s.accounts[i]))
	}
	return out
}

// Get 按 id 取账号副本。
func (s *Store) Get(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.accounts {
		if accountID(&s.accounts[i]) == id {
			return s.accounts[i], true
		}
	}
	return Account{}, false
}

// Upsert 按 user_id 合并写入一个账号（同 user_id 覆盖 token），并原子落盘。
func (s *Store) Upsert(a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(a.Token) == "" {
		return fmt.Errorf("empty token")
	}
	if a.ObtainedAtMS == 0 {
		a.ObtainedAtMS = time.Now().UnixMilli()
	}
	id := accountID(&a)
	for i := range s.accounts {
		if accountID(&s.accounts[i]) == id {
			// 保留原 label（若新条目没带）。
			if a.Label == "" {
				a.Label = s.accounts[i].Label
			}
			s.accounts[i] = a
			return s.saveLocked()
		}
	}
	s.accounts = append(s.accounts, a)
	return s.saveLocked()
}

// UpdateTokens 刷新指定账号的 token 字段并落盘（refresh 后调用）。
func (s *Store) UpdateTokens(id, token, refresh string, expireAtMS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if accountID(&s.accounts[i]) == id {
			s.accounts[i].Token = token
			if refresh != "" {
				s.accounts[i].RefreshToken = refresh
			}
			s.accounts[i].ExpireAtMS = expireAtMS
			return s.saveLocked()
		}
	}
	return fmt.Errorf("account not found: %s", id)
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir for auth file: %w", err)
	}
	b, err := json.MarshalIndent(s.accounts, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// accountID 生成稳定标识：user_id 优先，否则用 token 头尾指纹。
func accountID(a *Account) string {
	if id := strings.TrimSpace(a.UserID); id != "" {
		return "uid:" + id
	}
	t := a.Token
	if len(t) <= 12 {
		return "tok:" + t
	}
	return "tok:" + t[:6] + t[len(t)-4:]
}
