// Package pool 是账号池：在多个 Verdent 登录账号间轮换，带冷却/禁用状态机与持久化。
//
// 与旧版（api key 池）的区别：池里的单元是「账号 id」（accountID），
// 真正的 token 由 verdent.Store 持有并按需 refresh；池只负责调度与健康状态。
package pool

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Class 错误分类，驱动冷却时长。
type Class string

const (
	ClassOK          Class = "ok"
	ClassRateLimited Class = "rate_limited" // 429 / 20004 限流车道
	ClassNoCredit    Class = "no_credit"    // 额度耗尽（限免窗口用尽）
	ClassAuthDead    Class = "auth_dead"    // 401 token 失效且无法刷新
	ClassTransient   Class = "transient"    // 5xx / 网络错误
	ClassOther       Class = "other"
)

// Config 池行为参数。
type Config struct {
	ErrThreshold   int
	ErrCooldown    time.Duration
	RateCooldown   time.Duration
	CreditCooldown time.Duration
	SelectJitterMS int
}

func (c *Config) defaults() {
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
		c.CreditCooldown = 1 * time.Hour
	}
}

// State 单账号运行时状态（可序列化）。
type State struct {
	ID             string    `json:"id"`
	Label          string    `json:"label,omitempty"`
	Disabled       bool      `json:"disabled"`
	DisabledReason string    `json:"disabled_reason,omitempty"`
	CooldownUntil  time.Time `json:"cooldown_until,omitempty"`
	ErrCount       int       `json:"err_count"`
	ReqCount       int64     `json:"req_count"`
	OkCount        int64     `json:"ok_count"`
	LastUsed       time.Time `json:"last_used,omitempty"`
	LastErr        string    `json:"last_err,omitempty"`
	LastErrAt      time.Time `json:"last_err_at,omitempty"`
	TotalIn        int64     `json:"total_in,omitempty"`
	TotalOut       int64     `json:"total_out,omitempty"`
}

// idSource 账号 id 来源（由 verdent.Store 实现，避免循环依赖）。
type idSource interface {
	IDs() []string
}

// Pool 账号池。
type Pool struct {
	cfg    Config
	mu     sync.Mutex
	src    idSource
	states map[string]*State
	order  []string

	stickyMu sync.Mutex
	sticky   map[string]*stickyEntry

	stateFile string
	rng       *rand.Rand
}

type stickyEntry struct {
	id       string
	lastSeen time.Time
}

// New 构造池。
func New(cfg Config, src idSource, stateFile string) *Pool {
	cfg.defaults()
	p := &Pool{
		cfg:       cfg,
		src:       src,
		states:    make(map[string]*State),
		sticky:    make(map[string]*stickyEntry),
		stateFile: stateFile,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	p.loadState()
	p.reindex()
	return p
}

func (p *Pool) reindex() {
	ids := p.src.IDs()
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
		if _, ok := p.states[id]; !ok {
			p.states[id] = &State{ID: id}
		}
	}
	for id := range p.states {
		if !present[id] {
			delete(p.states, id)
		}
	}
	p.order = p.order[:0]
	for id := range p.states {
		p.order = append(p.order, id)
	}
	sort.Strings(p.order)
}

// Refresh 重新同步账号并落盘（登录写入新账号后调用）。
func (p *Pool) Refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reindex()
	_ = p.saveLocked()
}

// SetLabel 记录账号显示名（供 /status）。
func (p *Pool) SetLabel(id, label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.states[id]; ok {
		s.Label = label
	}
}

func (p *Pool) loadState() {
	if p.stateFile == "" {
		return
	}
	b, err := os.ReadFile(p.stateFile)
	if err != nil {
		return
	}
	var snap struct {
		States []State `json:"states"`
	}
	if json.Unmarshal(b, &snap) != nil {
		return
	}
	for i := range snap.States {
		s := snap.States[i]
		st := s
		p.states[s.ID] = &st
	}
}

func (p *Pool) saveLocked() error {
	if p.stateFile == "" {
		return nil
	}
	snap := struct {
		States  []State   `json:"states"`
		SavedAt time.Time `json:"saved_at"`
	}{States: make([]State, 0, len(p.states)), SavedAt: time.Now()}
	for _, id := range p.order {
		if s, ok := p.states[id]; ok {
			snap.States = append(snap.States, *s)
		}
	}
	b, err := json.MarshalIndent(snap, "", "  ")
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

// Snapshot 返回状态拷贝。
func (p *Pool) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, 0, len(p.order))
	for _, id := range p.order {
		if s, ok := p.states[id]; ok {
			out = append(out, *s)
		}
	}
	return out
}

func (p *Pool) healthy(s *State, now time.Time) bool {
	if s == nil || s.Disabled {
		return false
	}
	if !s.CooldownUntil.IsZero() && now.Before(s.CooldownUntil) {
		return false
	}
	return true
}

// ErrNoHealthy 全池无可用账号。
var ErrNoHealthy = fmt.Errorf("no healthy account")

// Select 选一个可用账号 id；粘性优先。
func (p *Pool) Select(stickyKey string) (string, error) {
	if stickyKey != "" {
		if id := p.lookupSticky(stickyKey); id != "" && p.idHealthy(id) {
			return id, nil
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	cands := make([]*State, 0, len(p.order))
	for _, id := range p.order {
		if s := p.states[id]; p.healthy(s, now) {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return "", ErrNoHealthy
	}
	// 加权随机：闲置越久权重越高 + 成功率补偿。
	weights := make([]float64, len(cands))
	for i, s := range cands {
		w := 1.0
		if !s.LastUsed.IsZero() {
			w += now.Sub(s.LastUsed).Minutes() / 60.0
		}
		if s.ReqCount > 0 {
			rate := float64(s.OkCount) / float64(s.ReqCount)
			if rate < 0.2 {
				rate = 0.2
			}
			w *= rate
		}
		if w <= 0 {
			w = 1e-6
		}
		weights[i] = w
	}
	chosen := cands[weightedPick(p.rng, weights)]
	if stickyKey != "" {
		p.rememberSticky(stickyKey, chosen.ID)
	}
	return chosen.ID, nil
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

func (p *Pool) idHealthy(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[id]
	return ok && p.healthy(s, time.Now())
}

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
	return e.id
}

func (p *Pool) rememberSticky(key, id string) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	p.sticky[key] = &stickyEntry{id: id, lastSeen: time.Now()}
	if len(p.sticky) > 2048 {
		for k, e := range p.sticky {
			if time.Since(e.lastSeen) > 30*time.Minute {
				delete(p.sticky, k)
			}
		}
	}
}

// Result 请求结果上报。
type Result struct {
	OK       bool
	Class    Class
	Message  string
	TokensIn int
	TokensOut int
}

// Record 记录结果并驱动状态机。
func (p *Pool) Record(id string, r Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[id]
	if !ok {
		return
	}
	now := time.Now()
	s.LastUsed = now
	s.ReqCount++
	if r.OK {
		s.OkCount++
		s.ErrCount = 0
		s.TotalIn += int64(r.TokensIn)
		s.TotalOut += int64(r.TokensOut)
		return
	}
	s.ErrCount++
	s.LastErr = truncateErr(r.Message)
	s.LastErrAt = now
	switch r.Class {
	case ClassAuthDead:
		s.Disabled = true
		s.DisabledReason = "auth_dead: " + truncateErr(r.Message)
	case ClassNoCredit:
		s.CooldownUntil = now.Add(p.cfg.CreditCooldown)
	case ClassRateLimited:
		s.CooldownUntil = now.Add(p.cfg.RateCooldown)
	default:
		if s.ErrCount >= p.cfg.ErrThreshold {
			s.CooldownUntil = now.Add(p.cfg.ErrCooldown)
		}
	}
	_ = p.saveLocked()
}

// Revive 手动恢复一个账号。
func (p *Pool) Revive(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[id]
	if !ok {
		return fmt.Errorf("account not in pool: %s", id)
	}
	s.Disabled = false
	s.DisabledReason = ""
	s.CooldownUntil = time.Time{}
	s.ErrCount = 0
	_ = p.saveLocked()
	return nil
}

func truncateErr(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
