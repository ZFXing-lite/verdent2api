package pool

import (
	"context"
	"testing"
	"time"

	"verdent2api/internal/upstream"
)

// fakeKeySource 内存凭证源，用于测试。
type fakeKeySource struct{ keys []string }

func (f *fakeKeySource) Keys() []string { return f.keys }

func newTestPool(keys ...string) *Pool {
	client := upstream.New("https://api.example.test", time.Second, time.Second, time.Second, "")
	p := New(Config{
		MaxInFlight:    2,
		ErrThreshold:   3,
		ErrCooldown:    time.Minute,
		RateCooldown:   30 * time.Second,
		CreditCooldown: time.Hour,
		SelectJitterMS: 0, // 测试关闭抖动，保证可预测
	}, client, &fakeKeySource{keys: keys}, "")
	return p
}

func TestSelect_Empty(t *testing.T) {
	p := newTestPool()
	if _, err := p.Select(context.Background(), ""); err != ErrNoHealthy {
		t.Fatalf("want ErrNoHealthy, got %v", err)
	}
}

func TestSelect_SingleAndSticky(t *testing.T) {
	p := newTestPool("sk-a")
	ctx := context.Background()

	k, err := p.Select(ctx, "session-1")
	if err != nil || k != "sk-a" {
		t.Fatalf("select #1 = %q, %v", k, err)
	}
	// 粘性命中：同一会话永远同一 key。
	for i := 0; i < 3; i++ {
		k2, err := p.Select(ctx, "session-1")
		if err != nil || k2 != "sk-a" {
			t.Fatalf("sticky select #%d = %q, %v", i, k2, err)
		}
	}
	// 新会话还是同一个 key（池里只有一个）。
	k3, err := p.Select(ctx, "session-2")
	if err != nil || k3 != "sk-a" {
		t.Fatalf("select new session = %q, %v", k3, err)
	}
}

func TestSelect_AllDisabled(t *testing.T) {
	p := newTestPool("sk-a", "sk-b")
	if err := p.Disable("sk-a", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := p.Disable("sk-b", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Select(context.Background(), ""); err != ErrNoHealthy {
		t.Fatalf("want ErrNoHealthy, got %v", err)
	}
}

func TestRecord_CooldownByClass(t *testing.T) {
	p := newTestPool("sk-a")
	ctx := context.Background()

	// 429 -> 短冷却。
	p.Record("sk-a", Result{OK: false, Class: upstream.ClassRateLimited, Message: "slow down"})
	if _, err := p.Select(ctx, ""); err != ErrNoHealthy {
		t.Fatalf("rate-limited key should be cooling down, got %v", err)
	}
	if s := p.Snapshot()[0]; s.CooldownUntil.IsZero() {
		t.Fatal("cooldown_until should be set")
	}

	// Revive 恢复。
	if err := p.Revive("sk-a"); err != nil {
		t.Fatal(err)
	}
	if k, err := p.Select(ctx, ""); err != nil || k != "sk-a" {
		t.Fatalf("after revive select = %q, %v", k, err)
	}
}

func TestRecord_KeyDeadDisables(t *testing.T) {
	p := newTestPool("sk-a")
	p.Record("sk-a", Result{OK: false, Class: upstream.ClassKeyDead, Message: "invalid"})
	s := p.Snapshot()[0]
	if !s.Disabled {
		t.Fatal("key_dead should disable the key")
	}
	if s.DisabledReason == "" {
		t.Fatal("disabled_reason should be recorded")
	}
	// 禁用后不可选。
	if _, err := p.Select(context.Background(), ""); err != ErrNoHealthy {
		t.Fatalf("disabled key should not be selected, got %v", err)
	}
}

func TestRecord_ErrThresholdCooldown(t *testing.T) {
	p := newTestPool("sk-a")
	// 阈值 3，连续失败 2 次不冷却。
	p.Record("sk-a", Result{OK: false, Class: upstream.ClassOther, Message: "e1"})
	p.Record("sk-a", Result{OK: false, Class: upstream.ClassOther, Message: "e2"})
	if _, err := p.Select(context.Background(), ""); err != nil {
		t.Fatalf("below threshold should still be selectable: %v", err)
	}
	// 第 3 次触发冷却。
	p.Record("sk-a", Result{OK: false, Class: upstream.ClassOther, Message: "e3"})
	if _, err := p.Select(context.Background(), ""); err != ErrNoHealthy {
		t.Fatalf("at threshold should be cooling down, got %v", err)
	}
}

func TestRecord_SuccessResetsErrors(t *testing.T) {
	p := newTestPool("sk-a")
	p.Record("sk-a", Result{OK: false, Class: upstream.ClassOther, Message: "e"})
	p.Record("sk-a", Result{OK: true, Usage: &upstream.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}})
	s := p.Snapshot()[0]
	if s.ErrCount != 0 {
		t.Fatalf("err_count should reset, got %d", s.ErrCount)
	}
	if s.OkCount != 1 || s.ReqCount != 2 {
		t.Fatalf("counters wrong: ok=%d req=%d", s.OkCount, s.ReqCount)
	}
	if s.TotalIn != 10 || s.TotalOut != 5 {
		t.Fatalf("token counters wrong: in=%d out=%d", s.TotalIn, s.TotalOut)
	}
}

func TestAcquireRelease_InFlight(t *testing.T) {
	p := newTestPool("sk-a", "sk-b")
	p.Acquire("sk-a")
	if got := InFlightCount("sk-a"); got != 1 {
		t.Fatalf("in flight = %d, want 1", got)
	}
	p.Release("sk-a")
	if got := InFlightCount("sk-a"); got != 0 {
		t.Fatalf("in flight = %d, want 0", got)
	}
}

func TestRefresh_PicksUpNewKeys(t *testing.T) {
	src := &fakeKeySource{keys: []string{"sk-a"}}
	client := upstream.New("https://api.example.test", time.Second, time.Second, time.Second, "")
	p := New(Config{}, client, src, "")
	if k, _ := p.Select(context.Background(), ""); k != "sk-a" {
		t.Fatalf("unexpected key %q", k)
	}
	// 模拟登录工具写入新 key。
	src.keys = append(src.keys, "sk-b")
	p.Refresh()
	got := make(map[string]bool)
	for i := 0; i < 40; i++ {
		k, err := p.Select(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		got[k] = true
	}
	if !got["sk-b"] {
		t.Fatal("new key sk-b never selected after refresh")
	}
}

func TestSelect_JitterSkipsRecent(t *testing.T) {
	// 开启抖动时，刚选中的 key 会被跳过，让流量分散。
	client := upstream.New("https://api.example.test", time.Second, time.Second, time.Second, "")
	p := New(Config{SelectJitterMS: 5000}, client, &fakeKeySource{keys: []string{"sk-a", "sk-b"}}, "")
	ctx := context.Background()
	k1, _ := p.Select(ctx, "")
	k2, _ := p.Select(ctx, "")
	if k1 == k2 {
		t.Fatalf("jitter should spread selection, got %q twice", k1)
	}
}
