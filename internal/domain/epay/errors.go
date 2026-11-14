// Package epay 实现 EPay 兼容层的纯协议部分：签名、验签、参数校验、类型映射。
//
// 分层纪律（设计 §7）：本包只做字符串与字节运算，不 import gin、gorm、context、
// database/sql，也不碰任何持久化对象。这样签名算法可以用官方报文做纯函数测试，
// 协议细节也不会渗进业务层。
package epay

import "errors"

var (
	// ErrSignature 验签不通过：待签串被改动、密钥不匹配，或 sign 字段格式非法。
	// 对外文案沿用 EPay 约定，不区分具体原因，避免给探测者反馈。
	ErrSignature = errors.New("签名错误")

	// ErrAmbiguousParam 参数含歧义分隔符或控制字符，导致待签串无法唯一还原参数边界。
	// 这属于给签名伪造开口子的输入（参数走私可改写金额），必须在入口直接拒绝（设计 §1）。
	// 触发条件分两类，共用同一个哨兵：分隔符（名含 '='/'&'、值含 '&'）与
	// 控制字符（名或值含 C0 0x00–0x1F / DEL 0x7F，见 params.go 的 hasControlByte）。
	// 对外必须与 ErrSignature 表现完全一致（同样 code=-1 一句话、同样耗时特征），
	// 否则会成为区分「参数有问题」与「密钥不对」的探针；内部日志分开记，且不得打印 key/sign 原文。
	ErrAmbiguousParam = errors.New("参数含歧义分隔符或控制字符")

	// ErrInvalidAmount 金额格式错误。EPay 的 money 是「元、最多两位小数」的字符串，
	// 原文要进签名域，所以不接受任何宽松解析。
	ErrInvalidAmount = errors.New("金额格式错误")

	// ErrUnsupportedType 支付方式未开通。
	// 不允许静默回落到微信/支付宝：挂机端只能监听这两个渠道的通知栏，
	// 静默回落会让用户付了钱但类型不匹配，导致到账无法核销（设计 §5.2）。
	ErrUnsupportedType = errors.New("支付方式未开通")

	// ErrStaleTimestamp 时间戳超出允许窗口，用于 V2 的重放防护（±300s）。
	ErrStaleTimestamp = errors.New("时间戳超出允许窗口")
)
