package merchant

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"testing"

	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
)

func TestProtocol(t *testing.T) {
	cases := []struct {
		protocol Protocol
		valid    bool
		text     string
		purpose  string
	}{
		{protocol: ProtocolNative, valid: false, text: "native", purpose: ""},
		{protocol: ProtocolEPayV1, valid: true, text: "epay.v1", purpose: "epay.v1.key"},
		{protocol: ProtocolEPayV2, valid: true, text: "epay.v2", purpose: "epay.v2.private"},
		{protocol: Protocol(7), valid: false, text: "unknown(7)", purpose: ""},
	}

	for _, item := range cases {
		if got := item.protocol.Valid(); got != item.valid {
			t.Errorf("%d.Valid() = %v，期望 %v", int8(item.protocol), got, item.valid)
		}
		if got := item.protocol.String(); got != item.text {
			t.Errorf("%d.String() = %q，期望 %q", int8(item.protocol), got, item.text)
		}
		if got := item.protocol.Purpose(); got != item.purpose {
			t.Errorf("%d.Purpose() = %q，期望 %q", int8(item.protocol), got, item.purpose)
		}
	}
}

// TestKeyAADMatchesSecretAAD 锁住「领域层拼 AAD」与「密码学层拼 AAD」是同一份字节。
// 两处各写一份正是 U 盘里最典型的坑：创建时能加密，轮换/解密时永远解不开。
func TestKeyAADMatchesSecretAAD(t *testing.T) {
	key := Key{MerchantID: 42, Protocol: ProtocolEPayV2, Generation: GenerationCurrent}

	want := secret.AAD(42, 2, 1, "epay.v2.private")
	if got := key.AAD(); !bytes.Equal(got, want) {
		t.Fatalf("Key.AAD 与 secret.AAD 不一致\n实际: % x\n期望: % x", got, want)
	}

	v1 := Key{MerchantID: 42, Protocol: ProtocolEPayV1, Generation: GenerationPrevious}
	if bytes.Equal(v1.AAD(), key.AAD()) {
		t.Fatal("V1 与 V2 的 AAD 相同，密文可被跨协议复用")
	}
}

func TestNewMerchantNo(t *testing.T) {
	const rounds = 100

	seen := make(map[string]struct{}, rounds)
	for i := 0; i < rounds; i++ {
		no, err := NewMerchantNo()
		if err != nil {
			t.Fatalf("生成商户号失败: %v", err)
		}
		if len(no) != merchantNoLength {
			t.Fatalf("商户号应为 %d 位，实际 %d 位：%q", merchantNoLength, len(no), no)
		}
		if no[0] != 'M' {
			t.Fatalf("商户号应以 M 开头，实际 %q", no)
		}
		for j := 1; j < len(no); j++ {
			if bytes.IndexByte([]byte(alphabet), no[j]) < 0 {
				t.Fatalf("商户号含字符集之外的字符 %q：%q", no[j], no)
			}
		}
		if _, ok := seen[no]; ok {
			t.Fatalf("商户号重复：%q", no)
		}
		seen[no] = struct{}{}
	}
}

func TestNewV1Secret(t *testing.T) {
	const rounds = 100

	seen := make(map[string]struct{}, rounds)
	for i := 0; i < rounds; i++ {
		v1, err := NewV1Secret()
		if err != nil {
			t.Fatalf("生成 V1 密钥失败: %v", err)
		}
		if len(v1) != V1SecretLength {
			t.Fatalf("V1 密钥应为 %d 位，实际 %d 位：%q", V1SecretLength, len(v1), v1)
		}
		if !IsValidV1Secret(v1) {
			t.Fatalf("生成的 V1 密钥未通过自检：%q", v1)
		}
		if _, ok := seen[v1]; ok {
			t.Fatalf("V1 密钥重复：%q", v1)
		}
		seen[v1] = struct{}{}
	}
}

func TestIsValidV1Secret(t *testing.T) {
	valid := alphabet + "a" // alphabet 是 31 位，补一位正好 32
	if len(valid) != V1SecretLength {
		t.Fatalf("测试向量长度应为 %d，实际 %d", V1SecretLength, len(valid))
	}
	if !IsValidV1Secret(valid) {
		t.Fatalf("合法密钥被判为非法：%q", valid)
	}

	cases := map[string]string{
		"过短":     valid[:31],
		"过长":     valid + "a",
		"含 0":    "0" + alphabet[1:] + "a",
		"含 1":    "1" + alphabet[1:] + "a",
		"含大写":    "A" + alphabet[1:] + "a",
		"含连字符":   alphabet + "-",
		"空":      "",
		"含 NUL":  alphabet + "\x00",
		"PEM 残片": "-----BEGIN PRIVATE KEY-----",
	}

	for name, value := range cases {
		if IsValidV1Secret(value) {
			t.Errorf("%s 的密钥应被判为非法：%q", name, value)
		}
	}
}

func TestHintV1(t *testing.T) {
	if got := HintV1(alphabet + "ab"); got != "89ab" {
		t.Fatalf("HintV1 应取末 4 位，实际 %q", got)
	}
	if got := HintV1("abc"); got != "abc" {
		t.Fatalf("短于 4 位时应原样返回，实际 %q", got)
	}
	if got := HintV1(""); got != "" {
		t.Fatalf("空串应返回空串，实际 %q", got)
	}
}

func TestHintPublicKey(t *testing.T) {
	privatePEM, publicPEM, err := NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成密钥对失败: %v", err)
	}
	if len(privatePEM) == 0 {
		t.Fatal("私钥 PEM 为空")
	}

	hint := HintPublicKey(publicPEM)
	if len(hint) != hintPublicKeyLength {
		t.Fatalf("公钥指纹应为 %d 位，实际 %d 位：%q", hintPublicKeyLength, len(hint), hint)
	}
	if _, err := hex.DecodeString(hint); err != nil {
		t.Fatalf("公钥指纹应为十六进制: %v", err)
	}
	if hint != HintPublicKey(publicPEM) {
		t.Fatal("同一公钥两次计算的指纹不一致")
	}

	// 公钥不同则指纹必须不同，否则管理端无法辨认商户贴的是哪一把。
	_, otherPEM, err := NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成密钥对失败: %v", err)
	}
	if hint == HintPublicKey(otherPEM) {
		t.Fatal("两把不同公钥得到了相同指纹")
	}
}

func TestRSAKeyPair(t *testing.T) {
	privatePEM, publicPEM, err := NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成密钥对失败: %v", err)
	}

	privateBlock, rest := pem.Decode(privatePEM)
	if privateBlock == nil || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("私钥 PEM 解码失败或含有冗余数据")
	}
	if privateBlock.Type != "PRIVATE KEY" {
		t.Fatalf("私钥 PEM 类型应为 PRIVATE KEY（PKCS#8），实际 %q", privateBlock.Type)
	}

	publicBlock, rest := pem.Decode(publicPEM)
	if publicBlock == nil || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("公钥 PEM 解码失败或含有冗余数据")
	}
	if publicBlock.Type != "PUBLIC KEY" {
		t.Fatalf("公钥 PEM 类型应为 PUBLIC KEY（PKIX），实际 %q", publicBlock.Type)
	}

	privateKey, err := x509.ParsePKCS8PrivateKey(privateBlock.Bytes)
	if err != nil {
		t.Fatalf("x509.ParsePKCS8PrivateKey 失败: %v", err)
	}
	private, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("PKCS#8 私钥应为 *rsa.PrivateKey，实际 %T", privateKey)
	}
	if bits := private.N.BitLen(); bits != rsaBits {
		t.Fatalf("私钥应为 %d 位，实际 %d 位", rsaBits, bits)
	}

	publicKey, err := x509.ParsePKIXPublicKey(publicBlock.Bytes)
	if err != nil {
		t.Fatalf("x509.ParsePKIXPublicKey 失败: %v", err)
	}
	public, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("PKIX 公钥应为 *rsa.PublicKey，实际 %T", publicKey)
	}
	if public.N.Cmp(private.N) != 0 || public.E != private.E {
		t.Fatal("公钥与私钥不配对")
	}

	// 闭环验签：商户拿平台公钥必须能验过本网关用平台私钥签的串。
	digest := sha256.Sum256([]byte("pid=M0000000000000&money=12.50&timestamp=1773500000"))
	signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("验签失败: %v", err)
	}

	tampered := sha256.Sum256([]byte("pid=M0000000000000&money=12.51&timestamp=1773500000"))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, tampered[:], signature); err == nil {
		t.Fatal("篡改后的待签串仍验签通过")
	}
}
