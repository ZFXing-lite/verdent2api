package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReload_PicksUpNewKeys 验证登录工具写入新 key 后 Reload 能重读文件。
func TestReload_PicksUpNewKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")

	// 起始空库（文件不存在）。
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(s.Keys()); got != 0 {
		t.Fatalf("expected empty store, got %d", got)
	}

	// 模拟另一进程（登录工具）写入一个 key。
	if err := os.WriteFile(path, []byte(`[{"api_key":"sk-new","label":"main"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	keys := s.Keys()
	if len(keys) != 1 || keys[0] != "sk-new" {
		t.Fatalf("reload did not pick up new key: %v", keys)
	}

	// 文件被清空后 Reload 应回到空库。
	if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload empty: %v", err)
	}
	if got := len(s.Keys()); got != 0 {
		t.Fatalf("expected empty after clear, got %d", got)
	}
}

// TestReload_BadJSONKeepsOldState 验证坏文件不会清空已有内存状态。
func TestReload_BadJSONKeepsOldState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(path, []byte(`[{"api_key":"sk-old"}]`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 写入半截 JSON（另一进程正在写）。
	if err := os.WriteFile(path, []byte(`[{"api_key":`), 0o600); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("expected parse error on bad JSON")
	}
	// 坏文件时保留旧状态，不清空。
	if keys := s.Keys(); len(keys) != 1 || keys[0] != "sk-old" {
		t.Fatalf("bad reload clobbered good state: %v", keys)
	}
}
