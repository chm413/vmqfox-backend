package port

import (
	"context"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/admin"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/qrcode"
)

var (
	ErrNotFound             = errorValue("记录不存在")
	ErrConflict             = errorValue("记录冲突")
	ErrPublicTokenConflict  = errorValue("公开订单令牌冲突")
	ErrAlreadyProcessed     = errorValue("事件已处理")
	ErrQRCodeNotFound       = errorValue("图片中未识别到二维码")
	ErrQRCodeUnsupported    = errorValue("二维码格式暂不支持")
	ErrQRCodeImageTooLarge  = errorValue("二维码图片尺寸过大")
	ErrQRCodeContentTooLong = errorValue("二维码内容过长")
)

type errorValue string

func (e errorValue) Error() string { return string(e) }

// Clock 和 OrderIDGenerator 将非确定性能力收敛在端口层。
type Clock interface {
	Now() time.Time
}

type OrderIDGenerator interface {
	NewOrderID(now time.Time) (string, error)
}

// PublicTokenGenerator 为公开订单生成不可枚举的随机令牌。
type PublicTokenGenerator interface {
	NewPublicToken() (string, error)
}

// TokenIssuer 只暴露登录所需的签发能力，认证实现不会泄漏到业务用例。
type TokenIssuer interface {
	Issue(subject string) (token string, expiresAt time.Time, err error)
}

type PasswordHasher interface {
	Hash(password string) (string, error)
	Verify(passwordHash, password string) (bool, error)
}

type AdminCredentialRepository interface {
	Get(ctx context.Context) (admin.Credential, error)
	GetForUpdate(ctx context.Context) (admin.Credential, error)
	Update(ctx context.Context, value admin.Credential) error
	Save(ctx context.Context, value admin.Credential) error
}

// TransactionManager 只负责事务边界。传入回调的 context 由适配器绑定事务连接。
type TransactionManager interface {
	WithinTransaction(ctx context.Context, fn func(context.Context) error) error
}

type SettingRepository interface {
	Get(ctx context.Context, key string) (string, error)
	GetMany(ctx context.Context, keys []string) (map[string]string, error)
	GetManyForUpdate(ctx context.Context, keys []string) (map[string]string, error)
	Set(ctx context.Context, key, value string) error
	SetMany(ctx context.Context, values map[string]string) error
}

type OrderFilter struct {
	State *order.Status
	Page  int
	Limit int
}

type OrderPage struct {
	Items []order.Order
	Total int64
}

type OrderStatistics struct {
	TodayOrders       int64
	TodayPaidOrders   int64
	TodayClosedOrders int64
	TodayPaidCents    int64
	TotalOrders       int64
	TotalPaidCents    int64
}

type OrderRepository interface {
	FindByID(ctx context.Context, id int64) (order.Order, error)
	FindByIDForUpdate(ctx context.Context, id int64) (order.Order, error)
	FindByOrderID(ctx context.Context, orderID string) (order.Order, error)
	FindByOrderIDForUpdate(ctx context.Context, orderID string) (order.Order, error)
	FindByPublicToken(ctx context.Context, token string) (order.Order, error)
	FindByPayID(ctx context.Context, payID string) (order.Order, error)
	FindByPayIDForUpdate(ctx context.Context, payID string) (order.Order, error)
	List(ctx context.Context, filter OrderFilter) (OrderPage, error)
	Statistics(ctx context.Context, dayStart, dayEnd time.Time) (OrderStatistics, error)
	Create(ctx context.Context, value order.Order) (order.Order, error)
	// FindPendingForUpdate 只匹配「付款发生之前创建」的待支付订单，避免后续同额订单吃掉历史付款。
	FindPendingForUpdate(ctx context.Context, typ payment.Type, reallyPriceCents int64, createdBefore time.Time) (order.Order, error)
	// FindReopenableForUpdate 匹配刚刚被超时关闭、但付款其实已经到账的订单，用于关闭/到账竞态的回补。
	FindReopenableForUpdate(ctx context.Context, typ payment.Type, reallyPriceCents int64, createdBefore, closedAfter time.Time) (order.Order, error)
	FindExpiredPendingForUpdate(ctx context.Context, before time.Time, limit int) ([]order.Order, error)
	Transition(ctx context.Context, id int64, from, to order.Status, changedAt time.Time) (bool, error)
	Delete(ctx context.Context, id int64) (bool, error)
	DeleteBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteExpiredBefore(ctx context.Context, before time.Time) (int64, error)
}

// QRCodeImageCodec 隔离二维码编解码库，业务层只处理字节和文本。
type QRCodeImageCodec interface {
	EncodePNG(content string) ([]byte, error)
	Decode(imageData []byte) (string, error)
}

type QRCodeFilter struct {
	Type  *payment.Type
	Page  int
	Limit int
}

type QRCodePage struct {
	Items []qrcode.QRCode
	Total int64
}

type QRCodeRepository interface {
	FindEnabledByAmount(ctx context.Context, typ payment.Type, amountCents int64) (qrcode.QRCode, error)
	FindByID(ctx context.Context, id int64) (qrcode.QRCode, error)
	List(ctx context.Context, filter QRCodeFilter) (QRCodePage, error)
	Create(ctx context.Context, value qrcode.QRCode) (qrcode.QRCode, error)
	SetState(ctx context.Context, id int64, from, to qrcode.State) (bool, error)
	Delete(ctx context.Context, id int64) (bool, error)
}

// PriceLockRepository 按支付类型和金额分占用一个可用收款金额。
type PriceLockRepository interface {
	TryAcquire(ctx context.Context, orderID string, paymentType payment.Type, amountCents int64) (bool, error)
	ReleaseByOrderID(ctx context.Context, orderID string) error
	// ReleaseByOrdersBefore 以订单为基准释放价格锁，保证「释放」与「删除订单」用的是同一批行。
	ReleaseByOrdersBefore(ctx context.Context, before time.Time) error
	// ReleaseOrphaned 清理已经没有关联订单的残留价格锁，避免金额被永久占死。
	ReleaseOrphaned(ctx context.Context) error
}

// PaymentEvent 保留 Android 原始时间戳，唯一 EventKey 用于抵御客户端重试造成的重复到账。
type PaymentEvent struct {
	EventKey    string
	PaymentType payment.Type
	PriceCents  int64
	EventTime   string
	CreatedAt   time.Time
}

type PaymentEventRepository interface {
	RecordIfNew(ctx context.Context, event PaymentEvent) (bool, error)
	// PruneBefore 清理过期的到账事件，防止幂等表无限增长。
	PruneBefore(ctx context.Context, before time.Time) (int64, error)
}

type OutboxMessage struct {
	ID            string
	Topic         string
	AggregateID   string
	Payload       []byte
	EventKey      string
	CreatedAt     time.Time
	Attempts      int
	NextAttemptAt time.Time
	LeaseToken    string
}

type OutboxRepository interface {
	Enqueue(ctx context.Context, message OutboxMessage) error
	ClaimPending(ctx context.Context, now time.Time, limit int, leaseDuration time.Duration) ([]OutboxMessage, error)
	MarkDelivered(ctx context.Context, id, leaseToken string, deliveredAt time.Time) error
	MarkFailed(ctx context.Context, id, leaseToken string, attempts int, nextAttemptAt time.Time, reason string) error
	// MarkDead 把超过重试上限的消息转为终态，停止继续重试。
	MarkDead(ctx context.Context, id, leaseToken string, attempts int, reason string) error
	// PruneDeliveredBefore 清理已投递的历史消息，避免 outbox 无上限增长。
	PruneDeliveredBefore(ctx context.Context, before time.Time) (int64, error)
}

type Notification struct {
	OrderID string
	URL     string
	Method  string
	Body    []byte
	Headers map[string]string
}

type NotificationResult struct {
	StatusCode int
	Body       []byte
}

type Notifier interface {
	Send(ctx context.Context, notification Notification) (NotificationResult, error)
}

// MerchantRepository 承载 EPay 多商户的商户主体（对内 id、对外 merchant_no）。
// 密钥不在本接口内：协议是否开通由 MerchantKeyRepository 有无当前代密钥自然决定。
type MerchantRepository interface {
	Create(ctx context.Context, value merchant.Merchant) (merchant.Merchant, error)
	FindByID(ctx context.Context, id int64) (merchant.Merchant, error)
	FindByNo(ctx context.Context, no string) (merchant.Merchant, error)
	List(ctx context.Context) ([]merchant.Merchant, error)
	Update(ctx context.Context, value merchant.Merchant) error
	Delete(ctx context.Context, id int64) error
	Count(ctx context.Context) (int64, error)
}

// MerchantKeyRepository 按「两代制」维护每商户每协议最多两行密钥：
// generation=1 用于签发与回调签名，generation=0 仅用于验签。
type MerchantKeyRepository interface {
	// FindActive 只取当前代；未命中返回 ErrNotFound（等价于该协议未开通）。
	FindActive(ctx context.Context, merchantID int64, protocol merchant.Protocol) (merchant.Key, error)
	// FindAllForVerify 返回当前代与上一代（当前代在前），供验签逐一尝试。
	// V2 必须用每行自己的 peer_public_key 验签，不能统一用当前代公钥。
	FindAllForVerify(ctx context.Context, merchantID int64, protocol merchant.Protocol) ([]merchant.Key, error)
	Upsert(ctx context.Context, value merchant.Key) error
	// Rotate 在一个事务里删上一代、把当前代降级为上一代、写入新当前代。
	Rotate(ctx context.Context, merchantID int64, protocol merchant.Protocol, next merchant.Key) error
	DeleteByMerchant(ctx context.Context, merchantID int64) error
}
