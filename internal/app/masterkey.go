package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/config"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// ErrMasterKeyMismatch 表示环境里的主密钥与数据库记录的指纹不是同一把。
//
// 这类故障必须在启动期硬失败：主密钥配错时服务能正常启动、界面一切照常，
// 只有商户在某个不经意的时刻发现「签名永远对不上」，排查成本极高。
// 保留成哨兵错误，便于运维按文案检索、也便于用例层/工具链用 errors.Is 判定。
var ErrMasterKeyMismatch = errors.New(
	"主密钥与数据库不匹配：请恢复原 VMQ_MASTER_KEY，或执行密钥重置（会要求全部商户重新配置）")

// errMasterKeyMissingBox 表示启用了主密钥链路却拿到空 Box，属于装配错误而非运维错误。
var errMasterKeyMissingBox = errors.New("主密钥未配置：无法执行指纹自检")

// masterKeyProbeTimeout 限制启动期对 settings 的读写耗时。
// 数据库半死不活时，卡在这里等 TCP 超时会让进程「起来了但一直不监听」，且没有任何日志；
// 加超时后至少能带着明确错误退出，让运维知道卡在自检环节。
const masterKeyProbeTimeout = 10 * time.Second

// ensureMasterKeyFingerprint 在启用主密钥时做启动自检。
//
// 返回值 write 的语义是「需要写入 settings.masterKeyFingerprint 的值」，
// 空字符串表示无需动作。决策表：
//
//	box 为 nil            -> 错误（调用方只有在启用主密钥后才应调用）
//	stored 为空           -> 返回当前指纹，调用方负责落库（首次启用）
//	stored 与当前指纹一致 -> 返回 ""，无需动作（正常运行）
//	stored 与当前指纹不同 -> 返回错误（换错主密钥/恢复了别的库）
//
// 只比当前写入版本，轮换期的旧键不参与比对：旧键只负责解开历史密文，
// 若把它算进来，运维清理 VMQ_MASTER_KEY_OLD 时就会被误判成「主密钥配错」。
func ensureMasterKeyFingerprint(stored string, box *secret.Box) (write string, err error) {
	if box == nil {
		return "", errMasterKeyMissingBox
	}

	current, err := box.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("计算主密钥自检指纹失败: %w", err)
	}

	// settings 的值可能由人工 SQL 或模板写入，尾随空白不该被当成「另一把主密钥」。
	stored = strings.TrimSpace(stored)
	switch {
	case stored == "":
		return current, nil
	case stored == current:
		return "", nil
	default:
		// 两个指纹本身非敏感（已落库且设计文档允许明文存放），带上便于运维确认是哪一侧配错。
		return "", fmt.Errorf("%w（库中指纹 %s，当前主密钥指纹 %s）", ErrMasterKeyMismatch, stored, current)
	}
}

// configureMasterKey 装配主密钥并在启动期完成指纹自检，返回的 Box 供后续商户密钥加解密使用。
//
// 三种启动形态（设计文档 §4.1）：
//  1. 启用主密钥：Box 就绪后做指纹自检，首次启用时写入指纹，不一致则拒绝启动；
//  2. 未启用但库里已有指纹：说明库中存在用该主密钥加密的商户密钥，把主密钥删掉等于自断解密钥，
//     必须拒绝启动；此时若只打 WARN，故障会推迟到某个商户下单时才爆出来；
//  3. 未启用且无指纹：存量部署升级路径，只打 WARN 并正常启动（零收益 breaking change 不可接受）。
func configureMasterKey(cfg config.MasterKeyConfig, settings port.SettingRepository, logger *log.Logger) (*secret.Box, error) {
	if logger == nil {
		logger = log.Default()
	}

	if !cfg.Enabled() {
		stored, err := readMasterKeyFingerprint(settings)
		if err != nil {
			// 未启用主密钥时读不到指纹不应阻断启动（例如全新库尚未建表），
			// 但必须留痕：此时「库里已有指纹」的守卫是关着的。
			logger.Printf("警告：读取主密钥自检指纹失败，跳过指纹守卫: %v", err)
			stored = ""
		}
		if stored != "" {
			return nil, fmt.Errorf(
				"%w（数据库中已存在指纹 %s，但当前未配置 VMQ_MASTER_KEY："+
					"库里的商户密钥将无法解密，请恢复原主密钥或执行密钥重置）",
				ErrMasterKeyMismatch, stored)
		}
		logger.Printf("警告：未配置 VMQ_MASTER_KEY，商户密钥相关功能不可用" +
			"（首次创建商户时会要求配置，生成命令：openssl rand -base64 32）")
		return nil, nil
	}

	box, err := secret.New(cfg.Keys, cfg.WriteVersion)
	if err != nil {
		return nil, fmt.Errorf("初始化主密钥失败: %w", err)
	}

	stored, err := readMasterKeyFingerprint(settings)
	if err != nil {
		// 启用主密钥后指纹是唯一的「库与密钥是否配套」证据，读不到就不许启动。
		return nil, err
	}

	write, err := ensureMasterKeyFingerprint(stored, box)
	if err != nil {
		return nil, err
	}
	if write != "" {
		ctx, cancel := context.WithTimeout(context.Background(), masterKeyProbeTimeout)
		defer cancel()
		if err := settings.Set(ctx, setting.MasterKeyFingerprintKey, write); err != nil {
			return nil, fmt.Errorf("写入主密钥自检指纹失败: %w", err)
		}
		logger.Printf("已写入主密钥自检指纹 %s（版本 %d）", write, cfg.WriteVersion)
	}

	return box, nil
}

// readMasterKeyFingerprint 读取自检指纹；键不存在时返回空串而不是 ErrNotFound，
// 因为「首次启用」与「从未启用」都是正常状态，调用方不该被迫区分两种错误。
func readMasterKeyFingerprint(settings port.SettingRepository) (string, error) {
	if settings == nil {
		return "", errors.New("settings 仓储未初始化")
	}

	ctx, cancel := context.WithTimeout(context.Background(), masterKeyProbeTimeout)
	defer cancel()

	values, err := settings.GetMany(ctx, []string{setting.MasterKeyFingerprintKey})
	if err != nil {
		if errors.Is(err, port.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("读取主密钥自检指纹失败: %w", err)
	}

	return strings.TrimSpace(values[setting.MasterKeyFingerprintKey]), nil
}
