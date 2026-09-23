package crypto

import (
	"encoding/json"
	"testing"
)

// pythonFixture 由参考实现（cryptography AESGCM）用固定 nonce=0..11 加密
// [{"role":"user","content":[{"type":"text","text":"hello 世界"}]}] 得到，
// 用于证明 Go 侧密钥派生 / GCM 参数与 Python 完全一致（可跨端互解）。
const pythonFixture = "AAECAwQFBgcICQoL3/uKhhvwR5cy4ydwBaVUAaYeAJnZG7aPzqJmca8HaNAFDaZAXeueA3J7gC+pMyLGfdjOSHMpgalCE2gytFBiDL0+u6TC47U353Bs5HBuBE7BVbrFV1VJ7HECyiAvUQQ="

func TestDecrypt_PythonFixture(t *testing.T) {
	var out []map[string]any
	if err := DecryptBlob(pythonFixture, &out); err != nil {
		t.Fatalf("decrypt python fixture: %v", err)
	}
	if len(out) != 1 || out[0]["role"] != "user" {
		t.Fatalf("unexpected decoded content: %+v", out)
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	obj := map[string]any{"a": 1, "b": "文字", "c": []int{1, 2, 3}}
	blob, err := EncryptObj(obj)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	var back map[string]any
	if err := DecryptBlob(blob, &back); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	// 数值经 JSON 往返成 float64，比较序列化结果即可。
	a, _ := json.Marshal(obj)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Fatalf("round-trip mismatch:\n  %s\n  %s", a, b)
	}
}
