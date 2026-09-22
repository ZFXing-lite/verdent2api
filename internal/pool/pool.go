// Package pool 是 API key 账号池：加权轮换 + 冷却/禁用状态机 + 状态持久化。
//
// 设计沿用 autoclaw2api / workbuddy2api 的语义：
//   - 选号 = 会话粘性（命中即定）→ 存活 key 加权随机（软均衡）
//   - 错误分类驱动冷却时长：429 短冷却、缺额长冷却、401 禁用需人工恢复
//   - 状态（计数/冷却/在途）原子落盘 state.json，重启不丢
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"verdent2api/internal/upstream"
)

// Config 池行为参数。
type Config struct {
	MaxInFlight    int           `json:"max_in_flight"`
	ErrThreshold   int           `json:"err_threshold"`
	ErrCooldown    time.Duration `json:"err_cooldown"`
	RateCooldown   time.Duration `json:"rate_cooldown"`
	CreditCooldown time.Duration `json:"credit_cooldown"`
	SelectJitterMS int           `json:"select_jitter_ms"`
}

func (c *Config) defaults() {
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 3
	}
	if c.ErrThreshold <= 0 {
		c.ErrThreshold = 5
	}
	if c.ErrCooldown <= 0 {
		c.ErrCooldown = 10 * time.Minute
	}
	if c.RateCooldown <= 0 {
		c.RateCooldown = 60 * time.Second
	}
	if c.CreditCooldown <= 0 {
		c.CreditCooldown = 12 * time.Hour
	}
}

// State 单个 key 的运行时状态（可序列化到 state.json）。
type State struct {
	APIKey         string    `json:"api_key"`
	Label          string    `json:"label,omitempty"`
	Disabled       bool      `json:"disabled"` // 人工/系统禁用（key 失效）
	DisabledReason string    `json:"disabled_reason,omitempty"`
	CooldownUntil  time.Time `json:"cooldown_until,omitempty"`
	ErrCount       int       `json:"err_count"`
	ReqCount       int64     `json:"req_count"`
	OkCount        int64     `json:"ok_count"`
	LastUsed       time.Time `json:"last_used,omitempty"`
	LastErr        string    `json:"last_err,omitempty"`
	LastErrAt      time.Time `json:"last_err_at,omitempty"`
	TotalIn        int64     `json:"total_in,omitempty"`  // 累计 prompt tokens
	TotalOut       int64     `json:"total_out,omitempty"` // 累计 completion tokens
}

// Pool 账号池。
type Pool struct {
	cfg    Config
	mu     sync.Mutex
	client *upstream.Client
	auth   keySource

	states map[string]*State
	order  []string // 稳定迭代序

	// 会话粘性：sessionKey -> apiKey，滚动续期。
	stickyMu sync.Mutex
	sticky   map[string]*stickyEntry

	stateFile string
	rng       *rand.Rand

	lastSelect   map[string]time.Time // 防惊群：key -> 上次被选中时刻
	lastSelectMu sync.Mutex
}

type stickyEntry struct {
	apiKey   string
	lastSeen time.Time
}

// keySource 凭证来源（由 internal/auth 实现，避免循环依赖）。
type keySource interface {
	Keys() []string
}

// New 构造池。keyFile 提供 key 清单，stateFile 持久化运行时状态。
func New(cfg Config, client *upstream.Client, keys keySource, stateFile string) *Pool {
	cfg.defaults()
	p := &Pool{
		cfg:        cfg,
		client:     client,
		auth:       keys,
		states:     make(map[string]*State),
		sticky:     make(map[string]*stickyEntry),
		stateFile:  stateFile,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
		lastSelect: make(map[string]time.Time),
	}
	p.loadState()
	p.reindex()
	return p
}

// reindex 把凭证源里的 key 同步进池（新增的初始化，删掉的标记移除）。
func (p *Pool) reindex() {
	keys := p.auth.Keys()
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		present[k] = true
		if _, ok := p.states[k]; !ok {
			p.states[k] = &State{APIKey: k}
		}
	}
	// 凭证里已删除的 key 从池里剔除（状态一起删，避免幽灵计数）。
	for k := range p.states {
		if !present[k] {
			delete(p.states, k)
		}
	}
	p.order = p.order[:0]
	for k := range p.states {
		p.order = append(p.order, k)
	}
	sort.Strings(p.order)
}

// Refresh 重新同步凭证并落盘。登录工具新增 key 后调用。
func (p *Pool) Refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reindex()
	_ = p.saveLocked()
}

// loadState 从 state.json 恢复（按 api_key 匹配）。
func (p *Pool) loadState() {
	if p.stateFile == "" {
		return
	}
	b, err := os.ReadFile(p.stateFile)
	if err != nil {
		return // 不存在/无权限都当空状态
	}
	var snapshot struct {
		States []State `json:"states"`
	}
	if json.Unmarshal(b, &snapshot) != nil {
		return
	}
	for i := range snapshot.States {
		s := snapshot.States[i]
		if _, ok := p.states[s.APIKey]; ok {
			st := s
			p.states[s.APIKey] = &st
		}
	}
}

// saveLocked 原子落盘（调用方持锁）。
func (p *Pool) saveLocked() error {
	if p.stateFile == "" {
		return nil
	}
	snapshot := struct {
		States  []State   `json:"states"`
		SavedAt time.Time `json:"saved_at"`
	}{
		States:  make([]State, 0, len(p.states)),
		SavedAt: time.Now(),
	}
	for _, k := range p.order {
		if s, ok := p.states[k]; ok {
			snapshot.States = append(snapshot.States, *s)
		}
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.stateFile), 0o755); err != nil {
		return err
	}
	tmp := p.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.stateFile)
}

// Snapshot 返回全部状态的拷贝（给 /status 用）。
func (p *Pool) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, 0, len(p.order))
	for _, k := range p.order {
		if s, ok := p.states[k]; ok {
			out = append(out, *s)
		}
	}
	return out
}

// healthy 返回该 key 当前是否可被选中。
func (p *Pool) healthy(s *State, now time.Time) bool {
	if s == nil || s.Disabled {
		return false
	}
	if !s.CooldownUntil.IsZero() && now.Before(s.CooldownUntil) {
		return false
	}
	return true
}

// candidates 计算存活候选并按权重排序。调用方持锁。
func (p *Pool) candidates(now time.Time) []*State {
	out := make([]*State, 0, len(p.order))
	for _, k := range p.order {
		s := p.states[k]
		if !p.healthy(s, now) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Select 按策略选一个可用 key；粘性优先。
// stickyKey 为会话级稳定键（如首条 user 消息 sha256）；空表示无粘性。
// 返回 (apiKey, error)。无可用 key 时返回 ErrNoHealthy。
func (p *Pool) Select(ctx context.Context, stickyKey string) (string, error) {
	// 粘性命中：同一会话固定同一 key，避免上下文跳号。
	if stickyKey != "" {
		if k := p.lookupSticky(stickyKey); k != "" && p.keyHealthy(k) {
			return k, nil
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	cands := p.candidates(now)
	if len(cands) == 0 {
		return "", ErrNoHealthy
	}

	// 防惊群：跳过 SelectJitterMS 内刚被选中的 key（除非它是唯一候选）。
	if jitter := time.Duration(p.cfg.SelectJitterMS) * time.Millisecond; jitter > 0 && len(cands) > 1 {
		p.lastSelectMu.Lock()
		filtered := cands[:0]
		for _, s := range cands {
			if last, ok := p.lastSelect[s.APIKey]; ok && now.Sub(last) < jitter {
				continue
			}
			filtered = append(filtered, s)
		}
		p.lastSelectMu.Unlock()
		if len(filtered) > 0 {
			cands = filtered
		}
	}

	// 在途租约上限：占满的号不参与。
	if p.cfg.MaxInFlight > 0 {
		free := make([]*State, 0, len(cands))
		for _, s := range cands {
			if inFlightGet(s.APIKey) < p.cfg.MaxInFlight {
				free = append(free, s)
			}
		}
		if len(free) > 0 {
			cands = free
		}
	}

	// 加权随机：闲置补偿（越久没用权重越高）+ 成功率补偿。
	var weights []float64
	for _, s := range cands {
		idle := now.Sub(s.LastUsed)
		w := 1.0
		if !s.LastUsed.IsZero() {
			w += idle.Minutes() / 60.0 // 每闲置 1 小时权重 +1
		}
		okRate := 1.0
		if s.ReqCount > 0 {
			okRate = float64(s.OkCount) / float64(s.ReqCount)
			if okRate < 0.2 {
				okRate = 0.2
			}
		}
		w *= okRate
		if w <= 0 {
			w = 1e-6
		}
		weights = append(weights, w)
	}

	pick := weightedPick(p.rng, weights)
	chosen := cands[pick]

	p.lastSelectMu.Lock()
	p.lastSelect[chosen.APIKey] = now
	p.lastSelectMu.Unlock()

	if stickyKey != "" {
		p.rememberSticky(stickyKey, chosen.APIKey)
	}
	return chosen.APIKey, nil
}

func weightedPick(r *rand.Rand, weights []float64) int {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return r.Intn(len(weights))
	}
	target := r.Float64() * total
	for i, w := range weights {
		target -= w
		if target <= 0 {
			return i
		}
	}
	return len(weights) - 1
}

// keyHealthy 查询指定 key 是否健康（不加池锁，供粘性路径用）。
func (p *Pool) keyHealthy(apiKey string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[apiKey]
	return ok && p.healthy(s, time.Now())
}

// lookupSticky 查会话粘性绑定（30 分钟滚动过期）。
func (p *Pool) lookupSticky(key string) string {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	e, ok := p.sticky[key]
	if !ok {
		return ""
	}
	if time.Since(e.lastSeen) > 30*time.Minute {
		delete(p.sticky, key)
		return ""
	}
	e.lastSeen = time.Now()
	return e.apiKey
}

func (p *Pool) rememberSticky(key, apiKey string) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	p.sticky[key] = &stickyEntry{apiKey: apiKey, lastSeen: time.Now()}
	// 粘性表膨胀保护：超过 2048 条时清理最旧的。
	if len(p.sticky) > 2048 {
		for k, e := range p.sticky {
			if time.Since(e.lastSeen) > 30*time.Minute {
				delete(p.sticky, k)
			}
		}
	}
}

// inFlight 计数（独立锁，避免与池锁嵌套）。
var (
	inFlightMu sync.Mutex
	inFlight   = make(map[string]int)
)

func inFlightAdd(k string, n int) {
	inFlightMu.Lock()
	inFlight[k] += n
	if inFlight[k] < 0 {
		inFlight[k] = 0
	}
	inFlightMu.Unlock()
}

func inFlightGet(k string) int {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	return inFlight[k]
}

// InFlightCount 返回某 key 当前在途请求数（给 /status 用）。
func InFlightCount(apiKey string) int {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	return inFlight[apiKey]
}

// Acquire 预占一个在途槽（请求开始时调用，结束必须配对 Release）。
func (p *Pool) Acquire(apiKey string) {
	inFlightAdd(apiKey, 1)
}

// Release 释放在途槽（纯计数，请求结束必须配对 Acquire 调用）。
func (p *Pool) Release(apiKey string) {
	inFlightAdd(apiKey, -1)
}

// Record 记录一次请求的结果并驱动状态机；一次请求只应调用一次。
func (p *Pool) Record(apiKey string, result Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[apiKey]
	if !ok {
		return
	}
	now := time.Now()
	s.LastUsed = now
	s.ReqCount++
	if result.OK {
		s.OkCount++
		s.ErrCount = 0
		if result.Usage != nil {
			s.TotalIn += int64(result.Usage.PromptTokens)
			s.TotalOut += int64(result.Usage.CompletionTokens)
		}
		return
	}
	s.ErrCount++
	s.LastErr = truncateErr(result.Message)
	s.LastErrAt = now

	switch result.Class {
	case upstream.ClassKeyDead:
		s.Disabled = true
		s.DisabledReason = "key_dead: " + truncateErr(result.Message)
	case upstream.ClassNoCredit:
		s.CooldownUntil = now.Add(p.cfg.CreditCooldown)
	case upstream.ClassRateLimited:
		s.CooldownUntil = now.Add(p.cfg.RateCooldown)
	default:
		if s.ErrCount >= p.cfg.ErrThreshold {
			s.CooldownUntil = now.Add(p.cfg.ErrCooldown)
		}
	}
	_ = p.saveLocked()
}

// truncateErr 截断错误信息避免 state.json 膨胀。
func truncateErr(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// Revive 手动恢复某个被禁用/冷却的 key。
func (p *Pool) Revive(apiKey string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[apiKey]
	if !ok {
		return fmt.Errorf("key not in pool: %s", maskKey(apiKey))
	}
	s.Disabled = false
	s.DisabledReason = ""
	s.CooldownUntil = time.Time{}
	s.ErrCount = 0
	_ = p.saveLocked()
	return nil
}

// Disable 手动禁用。
func (p *Pool) Disable(apiKey, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[apiKey]
	if !ok {
		return fmt.Errorf("key not in pool: %s", maskKey(apiKey))
	}
	s.Disabled = true
	s.DisabledReason = reason
	_ = p.saveLocked()
	return nil
}

// maskKey 脱敏（日志/status 用）。
func maskKey(k string) string {
	if len(k) <= 12 {
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}

// Result 请求结束上报。
type Result struct {
	OK      bool
	Class   upstream.Class
	Message string
	Usage   *upstream.Usage
}

// ErrNoHealthy 全池无可用 key。
var ErrNoHealthy = fmt.Errorf("no healthy account")
