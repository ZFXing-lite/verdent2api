// Package crypto 实现 Verdent llm-proxy 的 AES-256-GCM 请求信封。
//
// 逆向事实（取自桌面版 app.asar，与参考实现 ErfanBagheri404/Verdent2API 对拍一致）：
//   - 签名常量 SIGN = "codeck502deck_25_09_15v7"
//   - AES 密钥 = base64(SIGN) 的前 32 字节（正好 32 字符，无填充）
//   - 信封 = base64( nonce(12) | ciphertext | gcm_tag(16) )
//   - Go 的 gcm.Seal 输出 = ciphertext|tag，与 Python cryptography 的 AESGCM.encrypt 一致
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// sign 是桌面版硬编码的签名常量（密钥派生源）。
const sign = "codeck502deck_25_09_15v7"

// key 缓存派生出的 32 字节 AES-256 密钥。
var key = deriveKey()

// deriveKey 复刻 base64(SIGN)[:32]。
func deriveKey() []byte {
	b64 := base64.StdEncoding.EncodeToString([]byte(sign))
	return []byte(b64)[:32]
}

// EncryptObj 把任意对象 JSON 序列化后加密，返回 base64 信封字符串。
func EncryptObj(obj interface{}) (string, error) {
	plain, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshal plaintext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize()) // 12
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	// Seal 追加密文与 tag 到 nonce 后面，一次拼出 nonce|ct|tag。
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptBlob 解开信封（主要供测试与调试用）。
func DecryptBlob(b64 string, out interface{}) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return fmt.Errorf("blob too short")
	}
	plain, err := gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, out)
}
