package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Cipher 使用 AES-256-GCM 加密数据库中的 OAuth 凭据。
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher 从 Base64 编码的 32 字节密钥创建凭据加密器。
func NewCipher(encodedKey string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("解析凭据加密密钥: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("凭据加密密钥必须是 Base64 编码的 32 字节密钥")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt 加密敏感明文并返回 Base64 字符串。
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密数据库中的 OAuth 凭据。
func (c *Cipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("解析加密凭据: %w", err)
	}
	if len(data) < c.aead.NonceSize() {
		return "", fmt.Errorf("加密凭据长度无效")
	}
	nonce, ciphertext := data[:c.aead.NonceSize()], data[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("解密凭据: %w", err)
	}
	return string(plain), nil
}
