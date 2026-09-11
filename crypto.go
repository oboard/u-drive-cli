package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// crypto.go — 多用户加密原语。
//
// 密钥模型（「凭证即密钥」）：
//   - 加密密钥由 username+password 纯函数派生（PBKDF2-HMAC-SHA256）。
//   - 无独立 DEK、无 keyring、无需备份：任何机器只要账号密码正确就能算出同一密钥解密。
//   - 前提代价：改密码会派生新密钥，旧密文将不可读（不提供自动迁移）。

const (
	dekSize       = 32 // AES-256 密钥长度
	gcmNonceSize  = 12
	gcmTagSize    = 16
	kekIterations = 100_000
	blockSize     = 64 * 1024
	// blockLenSize 是每个加密块的长度前缀字节数。
	blockLenSize = 4
)

var errCorruptBlock = errors.New("密文块损坏")

// deriveKey 从 username+password 派生稳定密钥（PBKDF2-HMAC-SHA256）。
func deriveKey(username, password string) []byte {
	salt := sha256.Sum256([]byte("udrive-kek-v1\x00" + username))
	dkLen := dekSize
	prf := func(in []byte) []byte {
		h := hmac.New(sha256.New, []byte(username+"\x00"+password))
		h.Write(in)
		return h.Sum(nil)
	}

	dk := make([]byte, dkLen)
	T := make([]byte, 0, dkLen)
	counter := 1
	for len(T) < dkLen {
		u := append([]byte{}, salt[:]...)
		u = append(u, byte(counter>>24), byte(counter>>16), byte(counter>>8), byte(counter))
		u = prf(u)
		v := append([]byte{}, u...)
		for i := 1; i < kekIterations; i++ {
			u = prf(u)
			for j := 0; j < len(v); j++ {
				v[j] ^= u[j]
			}
		}
		T = append(T, v...)
		counter++
	}
	copy(dk, T[:dkLen])
	return dk
}

// generateKey 生成随机密钥（测试/演示用；生产密钥由 deriveKey 纯函数派生）。
func generateKey() ([]byte, error) {
	key := make([]byte, dekSize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// aeadSeal 整块 AEAD 加密，前置随机 nonce。
func aeadSeal(key, plaintext []byte) ([]byte, error) {
	var nonce [gcmNonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce[:], plaintext, nil)
	return append(nonce[:], ct...), nil
}

// aeadOpen 解包 aeadSeal 的密文。
func aeadOpen(key, packed []byte) ([]byte, error) {
	if len(packed) < gcmNonceSize+gcmTagSize {
		return nil, errors.New("密文过短")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, packed[:gcmNonceSize], packed[gcmNonceSize:], nil)
}

// --- 文件内容流式加解密（块=自定界） ---
//
// 每个密文块格式（大端长度前缀，块内独立随机 nonce）：
//   [4 字节 长度 L][nonce (12)][ciphertext (L-12)]
//
// L = 12(nonce) + len(ciphertext)，ciphertext = tag(16) + data。
// 用 4 字节长度前缀保证能容纳整块（64KiB + 非/头开销）。

// encryptReader 把明文 reader 按块加密输出；维护内部待输出缓冲以兼容任意 len(p)。
type encryptReader struct {
	src     io.Reader
	gcm     cipher.AEAD
	buf     []byte
	pending []byte
	srcEOF  bool
}

func newEncryptReader(reader io.Reader, key []byte) (*encryptReader, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &encryptReader{src: reader, gcm: gcm, buf: make([]byte, blockSize)}, nil
}

func (r *encryptReader) Read(p []byte) (int, error) {
	for {
		if len(r.pending) > 0 {
			n := copy(p, r.pending)
			r.pending = r.pending[n:]
			return n, nil
		}
		if r.srcEOF {
			return 0, io.EOF
		}
		n, err := r.src.Read(r.buf)
		if err != nil && err != io.EOF {
			return 0, err
		}
		if n == 0 && err == io.EOF {
			r.srcEOF = true
			return 0, io.EOF
		}
		var nonce [gcmNonceSize]byte
		if _, rerr := rand.Read(nonce[:]); rerr != nil {
			return 0, rerr
		}
		ct := r.gcm.Seal(nil, nonce[:], r.buf[:n], nil)
		out := make([]byte, 0, blockLenSize+gcmNonceSize+len(ct))
		var lenBuf [blockLenSize]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(gcmNonceSize+len(ct)))
		out = append(out, lenBuf[:]...)
		out = append(out, nonce[:]...)
		out = append(out, ct...)
		r.pending = append(r.pending, out...)
		if err == io.EOF {
			r.srcEOF = true
		}
	}
}

// decryptReader 把密文流按块解密成明文；内部维护 pending 缓冲。
type decryptReader struct {
	src     io.Reader
	gcm     cipher.AEAD
	pending []byte
	done    bool
}

func newDecryptReader(reader io.Reader, key []byte) (*decryptReader, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &decryptReader{src: reader, gcm: gcm}, nil
}

func (r *decryptReader) Read(p []byte) (int, error) {
	for {
		if len(r.pending) > 0 {
			n := copy(p, r.pending)
			r.pending = r.pending[n:]
			return n, nil
		}
		if r.done {
			return 0, io.EOF
		}
		var lenBuf [blockLenSize]byte
		if _, err := io.ReadFull(r.src, lenBuf[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				r.done = true
				return 0, io.EOF
			}
			return 0, err
		}
		blen := int(binary.BigEndian.Uint32(lenBuf[:]))
		payload := make([]byte, blen)
		if _, err := io.ReadFull(r.src, payload); err != nil {
			return 0, err
		}
		if blen < gcmNonceSize+gcmTagSize {
			return 0, errCorruptBlock
		}
		pt, err := r.gcm.Open(nil, payload[:gcmNonceSize], payload[gcmNonceSize:], nil)
		if err != nil {
			return 0, fmt.Errorf("解密失败: %w", err)
		}
		r.pending = append(r.pending, pt...)
		if len(pt) < blockSize {
			r.done = true
		}
	}
}
