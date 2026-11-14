package secret

import (
	"bytes"
	"errors"
	"testing"
)

const (
	testMerchantID = int64(9007199254740993) // 超过 2^53，用来钉住 AAD 里 merchantID 必须是完整 8 字节
	testProtocol   = int8(1)
	testPurpose    = "epay.v1.key"
)

func testKeys() map[int][]byte {
	return map[int][]byte{
		1: bytes.Repeat([]byte{0x11}, masterKeySize),
		2: bytes.Repeat([]byte{0x22}, masterKeySize),
	}
}

func mustBox(t *testing.T, keys map[int][]byte, writeVersion int) *Box {
	t.Helper()

	box, err := New(keys, writeVersion)
	if err != nil {
		t.Fatalf("构造 Box 失败: %v", err)
	}
	return box
}

func testAAD() []byte {
	return AAD(testMerchantID, testProtocol, testPurpose)
}

func TestSealOpenRoundTrip(t *testing.T) {
	box := mustBox(t, testKeys(), 2)
	plain := []byte("abcdefghjkmnpqrstuvwxyz23456789")

	cipherText, nonce, version, err := box.Seal(plain, testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	if version != 2 {
		t.Fatalf("写入版本应为 2，实际 %d", version)
	}
	if len(nonce) != nonceSize {
		t.Fatalf("nonce 应为 %d 字节，实际 %d", nonceSize, len(nonce))
	}
	// GCM 的认证标签固定 16 字节，密文长度不对说明 Seal 的调用参数被改过。
	if len(cipherText) != len(plain)+16 {
		t.Fatalf("密文应为明文 + 16 字节标签，实际 %d", len(cipherText))
	}
	if bytes.Contains(cipherText, plain) {
		t.Fatal("密文中出现了明文片段，加密未生效")
	}

	got, err := box.Open(cipherText, nonce, testAAD(), version)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("解出的明文与原文不一致\n实际: %q\n期望: %q", got, plain)
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	box := mustBox(t, testKeys(), 1)
	plain := []byte("same plaintext")

	first, firstNonce, _, err := box.Seal(plain, testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	second, secondNonce, _, err := box.Seal(plain, testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	if bytes.Equal(firstNonce, secondNonce) {
		t.Fatal("两次 Seal 的 nonce 相同，GCM 复用 nonce 会泄露明文异或值")
	}
	if bytes.Equal(first, second) {
		t.Fatal("同一明文两次加密得到相同密文，nonce 未被使用")
	}
}

func TestOpenWithWrongAAD(t *testing.T) {
	box := mustBox(t, testKeys(), 1)
	plain := []byte("abcdefghjkmnpqrstuvwxyz23456789")

	cipherText, nonce, version, err := box.Seal(plain, testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	cases := map[string][]byte{
		"换商户":   AAD(testMerchantID+1, testProtocol, testPurpose),
		"换协议":   AAD(testMerchantID, 2, testPurpose),
		"换用途":   AAD(testMerchantID, testProtocol, "epay.v2.private"),
		"空 AAD": nil,
		"截断 AAD": testAAD()[1:],
	}

	for name, aad := range cases {
		if _, err := box.Open(cipherText, nonce, aad, version); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s 后 Open 应返回 ErrDecrypt，实际 %v", name, err)
		}
	}
}

func TestOpenTampered(t *testing.T) {
	box := mustBox(t, testKeys(), 1)
	plain := []byte("abcdefghjkmnpqrstuvwxyz23456789")

	cipherText, nonce, version, err := box.Seal(plain, testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	flipped := func(src []byte, index int) []byte {
		out := append([]byte(nil), src...)
		out[index] ^= 0x01
		return out
	}

	cases := []struct {
		name       string
		cipherText []byte
		nonce      []byte
	}{
		{name: "密文首字节", cipherText: flipped(cipherText, 0), nonce: nonce},
		{name: "密文字节中间", cipherText: flipped(cipherText, len(cipherText)/2), nonce: nonce},
		{name: "认证标签", cipherText: flipped(cipherText, len(cipherText)-1), nonce: nonce},
		{name: "nonce", cipherText: cipherText, nonce: flipped(nonce, 0)},
		{name: "密文截断", cipherText: cipherText[:len(cipherText)-1], nonce: nonce},
		{name: "nonce 过短", cipherText: cipherText, nonce: nonce[:nonceSize-1]},
	}

	for _, item := range cases {
		if _, err := box.Open(item.cipherText, item.nonce, testAAD(), version); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s 被篡改后应返回 ErrDecrypt，实际 %v", item.name, err)
		}
	}
}

// TestUnknownKeyVersion 区分「版本未知」与「解密失败」：
// 前者是把运维引向环境/回滚问题，后者才是商户配置问题，两者文案完全不同。
func TestUnknownKeyVersion(t *testing.T) {
	sealer := mustBox(t, testKeys(), 1)
	cipherText, nonce, version, err := sealer.Seal([]byte("secret"), testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	reader := mustBox(t, map[int][]byte{5: bytes.Repeat([]byte{0x33}, masterKeySize)}, 5)
	_, err = reader.Open(cipherText, nonce, testAAD(), version)
	if !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("应返回 ErrUnknownKeyVersion，实际 %v", err)
	}
	if errors.Is(err, ErrDecrypt) {
		t.Fatal("版本未知不应同时表现为 ErrDecrypt，否则上层无法区分故障方向")
	}
}

// TestOldKeyFallback 模拟主密钥轮换期的双读：新密文用版本 2 写，旧密文仍能用版本 1 读。
func TestOldKeyFallback(t *testing.T) {
	box := mustBox(t, testKeys(), 2)

	// 轮换前的历史数据：当时写入版本还是 1。
	oldSealer := mustBox(t, testKeys(), 1)
	oldCipher, oldNonce, oldVersion, err := oldSealer.Seal([]byte("legacy-secret"), testAAD())
	if err != nil {
		t.Fatalf("Seal 旧密文失败: %v", err)
	}
	if oldVersion != 1 {
		t.Fatalf("旧密文版本应为 1，实际 %d", oldVersion)
	}

	// 轮换后新写入的数据必须落到当前版本。
	newCipher, newNonce, newVersion, err := box.Seal([]byte("fresh-secret"), testAAD())
	if err != nil {
		t.Fatalf("Seal 新密文失败: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("新密文版本应为 2，实际 %d", newVersion)
	}

	oldPlain, err := box.Open(oldCipher, oldNonce, testAAD(), oldVersion)
	if err != nil {
		t.Fatalf("旧版本密文应仍可解开: %v", err)
	}
	if string(oldPlain) != "legacy-secret" {
		t.Fatalf("旧密文解出的明文错误: %q", oldPlain)
	}

	newPlain, err := box.Open(newCipher, newNonce, testAAD(), newVersion)
	if err != nil {
		t.Fatalf("新版本密文应可解开: %v", err)
	}
	if string(newPlain) != "fresh-secret" {
		t.Fatalf("新密文解出的明文错误: %q", newPlain)
	}
}

func TestNewValidatesKeyLength(t *testing.T) {
	cases := []struct {
		name         string
		keys         map[int][]byte
		writeVersion int
		wantErr      bool
	}{
		{name: "空集合", keys: nil, writeVersion: 1, wantErr: true},
		{name: "密钥 16 字节", keys: map[int][]byte{1: bytes.Repeat([]byte{0x11}, 16)}, writeVersion: 1, wantErr: true},
		{name: "密钥 33 字节", keys: map[int][]byte{1: bytes.Repeat([]byte{0x11}, 33)}, writeVersion: 1, wantErr: true},
		{name: "旧键长度非法", keys: map[int][]byte{1: bytes.Repeat([]byte{0x11}, 32), 2: []byte("short")}, writeVersion: 1, wantErr: true},
		{name: "写入版本不存在", keys: map[int][]byte{1: bytes.Repeat([]byte{0x11}, 32)}, writeVersion: 2, wantErr: true},
		{name: "合法", keys: testKeys(), writeVersion: 2, wantErr: false},
	}

	for _, item := range cases {
		box, err := New(item.keys, item.writeVersion)
		if item.wantErr {
			if err == nil {
				t.Errorf("%s 应返回错误，实际构造成功", item.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s 不应返回错误: %v", item.name, err)
		}
		if box == nil {
			t.Errorf("%s 构造成功但返回 nil Box", item.name)
		}
	}
}

// TestNewCopiesKeys 保证调用方清空自己的切片不会把 Box 里的密钥一起抹掉。
func TestNewCopiesKeys(t *testing.T) {
	keys := testKeys()
	box := mustBox(t, keys, 1)

	for _, key := range keys {
		for i := range key {
			key[i] = 0x00
		}
	}
	for version := range keys {
		delete(keys, version)
	}

	plain := []byte("still-alive")
	cipherText, nonce, version, err := box.Seal(plain, testAAD())
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	got, err := box.Open(cipherText, nonce, testAAD(), version)
	if err != nil {
		t.Fatalf("调用方清空源切片后 Box 应仍可用: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("明文不一致: %q", got)
	}
}

// TestNilBox 覆盖「主密钥未配置」路径：此时 Box 为 nil，必须返回专用错误而不是空指针 panic。
func TestNilBox(t *testing.T) {
	var box *Box

	if _, _, _, err := box.Seal([]byte("x"), testAAD()); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("nil Box 的 Seal 应返回 ErrNoMasterKey，实际 %v", err)
	}
	if _, err := box.Open([]byte("x"), bytes.Repeat([]byte{0x00}, nonceSize), testAAD(), 1); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("nil Box 的 Open 应返回 ErrNoMasterKey，实际 %v", err)
	}
}

// TestAADLayout 钉死 AAD 的字节布局：它是持久化契约，改动会让所有存量密文解不开。
// 布局刻意不含 generation——轮换只改 generation 而不重加密密文，绑进去就等于让旧代密文自动失效。
func TestAADLayout(t *testing.T) {
	aad := AAD(testMerchantID, 2, "epay.v2.private")

	const fixedLen = 8 + 1 + 1 + 1
	if len(aad) != fixedLen+len("epay.v2.private") {
		t.Fatalf("AAD 长度应为 %d，实际 %d", fixedLen+len("epay.v2.private"), len(aad))
	}
	if want := []byte{0x00, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}; !bytes.Equal(aad[:8], want) {
		t.Fatalf("merchantID 应大端写在最前 8 字节\n实际: % x\n期望: % x", aad[:8], want)
	}
	if aad[8] != 0x00 || aad[9] != 2 || aad[10] != 0x00 {
		t.Fatalf("分隔符/协议字段布局不符: % x", aad[8:11])
	}
	if string(aad[11:]) != "epay.v2.private" {
		t.Fatalf("purpose 应为 ASCII 原文，实际 %q", aad[11:])
	}
}

func TestFingerprint(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, masterKeySize)

	first, err := Fingerprint(key)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if len(first) != fingerprintHexLen {
		t.Fatalf("指纹应为 %d 位十六进制，实际 %d", fingerprintHexLen, len(first))
	}

	again, err := Fingerprint(key)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if first != again {
		t.Fatal("同一主密钥两次计算的指纹不一致，自检会误报")
	}

	other, err := Fingerprint(bytes.Repeat([]byte{0x22}, masterKeySize))
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if first == other {
		t.Fatal("不同主密钥得到了相同指纹，自检无法发现配错")
	}

	// 指纹必须真的依赖密钥内容，而不是只依赖长度。
	sameLenDiff := bytes.Repeat([]byte{0x11}, masterKeySize)
	sameLenDiff[0] = 0x12
	tweaked, err := Fingerprint(sameLenDiff)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if tweaked == first {
		t.Fatal("改动一个字节后指纹未变化")
	}

	if _, err := Fingerprint(bytes.Repeat([]byte{0x11}, 16)); err == nil {
		t.Fatal("长度非 32 字节的主密钥应报错")
	}
}
