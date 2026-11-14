package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/hulisang/vmqfox-backend/internal/config"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

const testMasterKeyVersion = 1

// fakeSettings 只实现自检用到的两个方法，其余方法留给接口占位；
// 用结构体而非 mock 库，是为了让「写入了什么」在断言里一眼可见。
type fakeSettings struct {
	values map[string]string
	getErr error
	setErr error
	writes map[string]string
}

func newFakeSettings(values map[string]string) *fakeSettings {
	if values == nil {
		values = map[string]string{}
	}
	return &fakeSettings{values: values, writes: map[string]string{}}
}

func (f *fakeSettings) Get(_ context.Context, key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	value, ok := f.values[key]
	if !ok {
		return "", errors.New("记录不存在")
	}
	return value, nil
}

func (f *fakeSettings) GetMany(_ context.Context, keys []string) (map[string]string, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := f.values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

func (f *fakeSettings) GetManyForUpdate(ctx context.Context, keys []string) (map[string]string, error) {
	return f.GetMany(ctx, keys)
}

func (f *fakeSettings) Set(_ context.Context, key, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.writes[key] = value
	f.values[key] = value
	return nil
}

func (f *fakeSettings) SetMany(ctx context.Context, values map[string]string) error {
	for key, value := range values {
		if err := f.Set(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

var _ port.SettingRepository = (*fakeSettings)(nil)

// newTestMasterKeyBox 构造一把长度合法的主密钥 Box；字节内容固定，保证指纹可复现。
func newTestMasterKeyBox(t *testing.T) *secret.Box {
	t.Helper()

	box, err := secret.New(map[int][]byte{testMasterKeyVersion: bytes.Repeat([]byte{0x11}, 32)}, testMasterKeyVersion)
	if err != nil {
		t.Fatalf("构造测试主密钥失败: %v", err)
	}
	return box
}

func TestEnsureMasterKeyFingerprint(t *testing.T) {
	box := newTestMasterKeyBox(t)
	current, err := box.Fingerprint()
	if err != nil {
		t.Fatalf("计算测试指纹失败: %v", err)
	}
	if len(current) != 16 {
		t.Fatalf("指纹长度应为 16 位十六进制，实际 %d: %q", len(current), current)
	}

	t.Run("首次启用时返回待写入的指纹", func(t *testing.T) {
		write, err := ensureMasterKeyFingerprint("", box)
		if err != nil {
			t.Fatalf("空指纹应视为首次启用，实际返回错误: %v", err)
		}
		if write != current {
			t.Fatalf("应返回当前指纹 %q，实际 %q", current, write)
		}
	})

	t.Run("指纹一致时无需动作", func(t *testing.T) {
		write, err := ensureMasterKeyFingerprint(current, box)
		if err != nil {
			t.Fatalf("指纹一致不应报错，实际: %v", err)
		}
		if write != "" {
			t.Fatalf("指纹一致时不应要求写入，实际返回 %q", write)
		}
	})

	t.Run("指纹不一致时拒绝启动", func(t *testing.T) {
		other, err := secret.New(map[int][]byte{testMasterKeyVersion: bytes.Repeat([]byte{0x22}, 32)}, testMasterKeyVersion)
		if err != nil {
			t.Fatalf("构造另一把主密钥失败: %v", err)
		}
		otherFingerprint, err := other.Fingerprint()
		if err != nil {
			t.Fatalf("计算另一把主密钥指纹失败: %v", err)
		}

		write, err := ensureMasterKeyFingerprint(otherFingerprint, box)
		if err == nil {
			t.Fatal("指纹不一致必须报错，否则会带着错的主密钥继续跑")
		}
		if write != "" {
			t.Fatalf("指纹不一致时不应返回待写入值，实际 %q", write)
		}
		if !errors.Is(err, ErrMasterKeyMismatch) {
			t.Fatalf("应返回 ErrMasterKeyMismatch，实际: %v", err)
		}
		// 文案必须可操作：只说「不匹配」会让运维去翻代码，必须给出两条出路。
		for _, want := range []string{"VMQ_MASTER_KEY", "密钥重置"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("错误文案应包含 %q，实际: %v", want, err)
			}
		}
	})

	t.Run("Box 为 nil 时拒绝自检", func(t *testing.T) {
		write, err := ensureMasterKeyFingerprint(current, nil)
		if err == nil {
			t.Fatal("Box 为 nil 时必须报错，否则等于跳过自检")
		}
		if !errors.Is(err, errMasterKeyMissingBox) {
			t.Fatalf("应返回 errMasterKeyMissingBox，实际: %v", err)
		}
		if write != "" {
			t.Fatalf("Box 为 nil 时不应返回待写入值，实际 %q", write)
		}
	})

	t.Run("前后空白不视为另一把主密钥", func(t *testing.T) {
		write, err := ensureMasterKeyFingerprint("  "+current+"\n", box)
		if err != nil {
			t.Fatalf("尾部空白不应导致误判，实际: %v", err)
		}
		if write != "" {
			t.Fatalf("空白处理后一致时不应要求写入，实际 %q", write)
		}
	})
}

// TestEnsureMasterKeyFingerprintIgnoresOldKey 确认轮换期旧键不影响自检结论：
// 旧键只用于解开历史密文，把它纳入比对会让清理 VMQ_MASTER_KEY_OLD 变成「主密钥配错」。
func TestEnsureMasterKeyFingerprintIgnoresOldKey(t *testing.T) {
	keys := map[int][]byte{
		1: bytes.Repeat([]byte{0x33}, 32), // 旧键
		2: bytes.Repeat([]byte{0x44}, 32), // 当前写入版本
	}
	box, err := secret.New(keys, 2)
	if err != nil {
		t.Fatalf("构造轮换期 Box 失败: %v", err)
	}

	current, err := box.Fingerprint()
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}

	oldOnly, err := secret.Fingerprint(keys[1])
	if err != nil {
		t.Fatalf("计算旧键指纹失败: %v", err)
	}
	if oldOnly == current {
		t.Fatal("测试前提失效：新旧键指纹不应相同")
	}

	write, err := ensureMasterKeyFingerprint(current, box)
	if err != nil {
		t.Fatalf("与当前版本一致不应报错，实际: %v", err)
	}
	if write != "" {
		t.Fatalf("与当前版本一致时不应要求写入，实际 %q", write)
	}

	if _, err := ensureMasterKeyFingerprint(oldOnly, box); !errors.Is(err, ErrMasterKeyMismatch) {
		t.Fatalf("库中若只记着旧键指纹应判定不匹配，实际: %v", err)
	}
}

// silentLogger 丢弃日志：装配测试只关心行为，日志文案由人工评审保证。
func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func testMasterKeyConfig() config.MasterKeyConfig {
	return config.MasterKeyConfig{
		Keys:         map[int][]byte{testMasterKeyVersion: bytes.Repeat([]byte{0x11}, 32)},
		WriteVersion: testMasterKeyVersion,
	}
}

// TestConfigureMasterKeyWiring 覆盖表里的三种启动形态，验证自检真的接进了装配流程：
// 只写 settings、不真正加解密，所以不需要数据库。
func TestConfigureMasterKeyWiring(t *testing.T) {
	cfg := testMasterKeyConfig()
	expected, err := secret.New(cfg.Keys, cfg.WriteVersion)
	if err != nil {
		t.Fatalf("构造期望 Box 失败: %v", err)
	}
	fingerprint, err := expected.Fingerprint()
	if err != nil {
		t.Fatalf("计算期望指纹失败: %v", err)
	}

	t.Run("启用主密钥且库中无指纹时写入指纹", func(t *testing.T) {
		settings := newFakeSettings(nil)

		box, err := configureMasterKey(cfg, settings, silentLogger())
		if err != nil {
			t.Fatalf("首次启用不应报错，实际: %v", err)
		}
		if box == nil {
			t.Fatal("首次启用后必须返回可用的 Box")
		}
		if got := settings.writes[setting.MasterKeyFingerprintKey]; got != fingerprint {
			t.Fatalf("应写入指纹 %q，实际写入 %q", fingerprint, got)
		}
	})

	t.Run("启用主密钥且库中指纹一致时不重复写入", func(t *testing.T) {
		settings := newFakeSettings(map[string]string{setting.MasterKeyFingerprintKey: fingerprint})

		box, err := configureMasterKey(cfg, settings, silentLogger())
		if err != nil {
			t.Fatalf("指纹一致不应报错，实际: %v", err)
		}
		if box == nil {
			t.Fatal("指纹一致时必须返回可用的 Box")
		}
		if len(settings.writes) != 0 {
			t.Fatalf("指纹一致时不应写入，实际写入了 %v", settings.writes)
		}
	})

	t.Run("启用主密钥但库中指纹不一致时拒绝启动", func(t *testing.T) {
		settings := newFakeSettings(map[string]string{setting.MasterKeyFingerprintKey: "0011223344556677"})

		box, err := configureMasterKey(cfg, settings, silentLogger())
		if !errors.Is(err, ErrMasterKeyMismatch) {
			t.Fatalf("应返回 ErrMasterKeyMismatch，实际: %v", err)
		}
		if box != nil {
			t.Fatal("指纹不一致时不得返回 Box，否则会带着错密钥继续跑")
		}
		if len(settings.writes) != 0 {
			t.Fatalf("指纹不一致时不应覆盖库中指纹，实际写入了 %v", settings.writes)
		}
	})

	t.Run("未启用主密钥但库中已有指纹时拒绝启动", func(t *testing.T) {
		settings := newFakeSettings(map[string]string{setting.MasterKeyFingerprintKey: fingerprint})

		box, err := configureMasterKey(config.MasterKeyConfig{}, settings, silentLogger())
		if !errors.Is(err, ErrMasterKeyMismatch) {
			t.Fatalf("库中有指纹却未配主密钥时必须报错，实际: %v", err)
		}
		if box != nil {
			t.Fatal("未启用主密钥时不应返回 Box")
		}
	})

	t.Run("未启用主密钥且库中无指纹时只告警并放行", func(t *testing.T) {
		settings := newFakeSettings(nil)

		box, err := configureMasterKey(config.MasterKeyConfig{}, settings, silentLogger())
		if err != nil {
			t.Fatalf("存量部署升级路径不应报错，实际: %v", err)
		}
		if box != nil {
			t.Fatal("未启用主密钥时不应返回 Box")
		}
		if len(settings.writes) != 0 {
			t.Fatalf("未启用主密钥时不应写入指纹，实际写入了 %v", settings.writes)
		}
	})

	t.Run("启用主密钥但读取指纹失败时拒绝启动", func(t *testing.T) {
		settings := newFakeSettings(nil)
		settings.getErr = errors.New("连接被拒绝")

		box, err := configureMasterKey(cfg, settings, silentLogger())
		if err == nil {
			t.Fatal("启用主密钥后读不到指纹必须拒绝启动")
		}
		if box != nil {
			t.Fatal("读取失败时不应返回 Box")
		}
	})

	t.Run("首次启用时写入指纹失败则拒绝启动", func(t *testing.T) {
		settings := newFakeSettings(nil)
		settings.setErr = errors.New("写库失败")

		box, err := configureMasterKey(cfg, settings, silentLogger())
		if err == nil {
			t.Fatal("指纹落库失败必须拒绝启动，否则下次启动的比对守卫形同不存在")
		}
		if box != nil {
			t.Fatal("写入失败时不应返回 Box")
		}
	})
}
