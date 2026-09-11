package main

import (
	"bytes"
	"io"
	"testing"
)

func TestDeriveKeyStableByPassword(t *testing.T) {
	k1 := deriveKey("alice", "password123")
	k2 := deriveKey("alice", "password123")
	if !bytes.Equal(k1, k2) {
		t.Error("同一 username+password 应派生同一密钥")
	}
	if len(k1) != dekSize {
		t.Errorf("密钥长度 = %d, want %d", len(k1), dekSize)
	}
	// 纯函数：无需持久化任何 key，任何进程/机器用相同账号密码即得同一密钥。
	k3 := deriveKey("alice", "password456")
	if bytes.Equal(k1, k3) {
		t.Error("不同 password 应派生不同密钥")
	}
	k4 := deriveKey("bob", "password123")
	if bytes.Equal(k1, k4) {
		t.Error("不同 username 应派生不同密钥")
	}
}

func TestEncryptStreamRoundtrip(t *testing.T) {
	key := deriveKey("alice", "secret")

	// 覆盖 空文件 / 小块 / 大块（多块）
	cases := []int{0, 1, 100, blockSize, blockSize + 1, blockSize*3 + 123}
	for _, size := range cases {
		plain := make([]byte, size)
		for i := range plain {
			plain[i] = byte(i)
		}
		enc, err := newEncryptReader(bytes.NewReader(plain), key)
		if err != nil {
			t.Fatal(err)
		}
		var ctBuf bytes.Buffer
		if _, err := io.Copy(&ctBuf, enc); err != nil {
			t.Fatalf("encrypt size=%d: %v", size, err)
		}

		dec, err := newDecryptReader(&ctBuf, key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(dec)
		if err != nil {
			t.Fatalf("decrypt size=%d: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("size=%d roundtrip mismatch", size)
		}
	}
}

func TestEncryptStreamRejectsBadKey(t *testing.T) {
	key := deriveKey("alice", "rightpass")
	plain := []byte("some content here")
	enc, _ := newEncryptReader(bytes.NewReader(plain), key)
	var ctBuf bytes.Buffer
	if _, err := io.Copy(&ctBuf, enc); err != nil {
		t.Fatal(err)
	}

	badKey := deriveKey("alice", "wrongpass")
	dec, _ := newDecryptReader(&ctBuf, badKey)
	if _, err := io.ReadAll(dec); err == nil {
		t.Error("错误密码（错误密钥）解密应失败")
	}
}

func TestWholeBlobSealOpenWithPassword(t *testing.T) {
	key := deriveKey("alice", "secret")
	doc := []byte(`{"entries": {}}`)
	sealed, err := aeadSeal(key, doc)
	if err != nil {
		t.Fatal(err)
	}
	// 密文应为随机化（nonce），不等于明文
	if bytes.Contains(sealed, doc) {
		t.Error("密文不应包含明文")
	}
	opened, err := aeadOpen(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, doc) {
		t.Error("解密不应与明文一致")
	}
}
