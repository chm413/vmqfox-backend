# EPay 兼容与多商户支持 · 设计方案

> 状态：待评审
> 目标版本：V3.3
> 前置事实：本网关当前为**单商户**（`admin_credentials.singleton_guard` 在 DB 层强制唯一），
> 支付方式仅 `payment.Wechat=1` / `payment.Alipay=2`，到账识别依赖挂机端按**金额**全局匹配。

---

## 0. 决策摘要

| 项 | 决策 |
| :-- | :-- |
| 协议角色 | 本网关**扮演 EPay 平台侧**，对外提供 `submit.php` / `mapi.php` / `api.php` 与 V2 的 `/api/pay/*` |
| 商户模型 | **多商户**，独立 `pid` + 密钥 + 回调地址，订单按商户隔离 |
| 协议版本 | **V1（MD5）与 V2（SHA256WithRSA）同时实现**，按商户配置决定启用哪套 |
| VMQ 原生接口 | **保留不动**，与 EPay 双协议并存 |
| 收款码 | 所有商户**共用同一批** `qrcodes`，这是免签系统的物理前提 |
| 金额锁 | `price_locks` 主键**保持全局唯一**，不引入商户维度（物理约束，见 §3.1） |
| 回调投递 | **复用现有 Outbox**，`notificationSucceeded` 已兼容 `success` 应答，投递层零改动 |

---

## 0.1 上游现状（设计起点，必须先看）

上游 Wiki 的 [`epay插件`](https://github.com/hulisang/vmqfox-backend/wiki/epay%E6%8F%92%E4%BB%B6) 页说明，
当前对接 EPay 生态的方式是**在 EPay 站点侧安装 `vmqfox_plugin.php`**，由 EPay 站点把 vmqfox
当作一条上游通道调用。插件参数是 `appurl`（vmqfox 地址）、`appid`（不校验，占位）、
`appkey`（= vmqfox 的通讯密钥），走的是 **VMQ 自己的 v2 HMAC-SHA-256**。

也就是说：**现有方案里，EPay 站点是调用方，vmqfox 是被调用方，但协议不是 EPay 标准协议。**

由此产生三个实际限制：

1. 站点必须能装 PHP 插件。SaaS 型发卡站、托管型面板装不了。
2. 必须替换默认的彩虹易支付插件，运营者要动服务器文件。
3. Wiki 明确写着：**「其余插件若仍使用 v1 MD5 签名将无法建单」**——
   独角数卡、异次元发卡的默认支付插件恰好都是 v1 MD5，直接对接会失败。

本次要做的，是补上**另一个方向**：让 vmqfox 自己扮演 EPay 平台，
站点只需在支付配置里填「网关地址 / PID / 密钥」三个格子，不装任何文件。

双轨并存后的形态：

| 对接方式 | 协议 | 站点侧改动 | 适用对象 |
| :-- | :-- | :-- | :-- |
| 现有 `vmqfox_plugin.php` | VMQ v2 HMAC-SHA-256 | 上传 PHP 插件、填 appurl/appid/appkey | 有服务器权限的自建站点 |
| **本次新增 · EPay V1** | EPay 标准 MD5 + `submit.php`/`mapi.php`/`api.php` | 只填三格，不动文件 | 独角数卡、异次元发卡等存量插件 |
| **本次新增 · EPay V2** | EPay 标准 SHA256WithRSA + `/api/pay/*` | 填三格 + 双方交换公钥 | 支持 RSA 的新版站点 |

三者共用同一套订单、金额锁、收款码与挂机端，互不干扰。

### 0.2 术语澄清：三个「v2」不要混

本文档里出现的「V2」有三种，含义完全不同：

| 写法 | 指什么 | 签名 | 现状 |
| :-- | :-- | :-- | :-- |
| **EPay V1** | 本文新增，易支付旧版商户协议 | MD5，密钥拼在待签串末尾 | 待实现 |
| **EPay V2** | 本文新增，易支付新版商户协议 | SHA256WithRSA 双向 + `timestamp` | 待实现 |
| **VMQ v2** | 仓库现有，`/api/order/create` 等用的协议 | HMAC-SHA-256，密钥作 HMAC key | 已在生产运行 |

`payment.SignVersion2`、`CreateSignV2`、Wiki 的「签名协议 v2」、以及报错文案
「检测到已废弃的 v1 签名」里的 v1/v2，**全部指第三行**，与 EPay 的协议版本无关。
新增代码中一律用 `EPayV1` / `EPayV2` / `VMQv2` 三个前缀显式命名，禁止裸写 `v1`/`v2`。

---

## 1. 为什么不能简单加个路由

EPay 看起来只是「换一套参数名」，实际有三个不能绕的硬约束：

**① 签名算法完全不同，且不可调和**

| | VMQ v2（现状） | EPay V1 | EPay V2 |
| :-- | :-- | :-- | :-- |
| 算法 | HMAC-SHA-256 | MD5 | SHA256WithRSA |
| 密钥参与方式 | 作为 HMAC key，不进明文 | **直接拼在待签串末尾** | 私钥签名 / 公钥验签 |
| 参数顺序 | 固定顺序，不排序 | **按 key 的 ASCII 升序** | 同 V1 |
| 空值 | 保留 | **剔除** | 剔除 |
| 金额格式 | 两位小数规范化 | **用原始字符串，不得重新格式化** | 同 V1 |
| 重放防护 | 无 | 无 | `timestamp` ±300s |

V1 的待签串构造必须逐字节复刻，任何"顺手规范化"都会导致第三方验签失败：

```
1. 剔除 sign、sign_type
2. 剔除值为空串的参数
3. ksort(SORT_STRING) 按 key 的 ASCII 升序
4. key=value 用 & 连接（不做 URL 编码、不去空格）
5. 末尾直接追加商户 key（无 & 、无 key= 前缀）
6. md5() 取小写十六进制
```

**② 回调语义不同，但恰好能复用**

EPay 回调要求商户返回纯文本 `success`。现有 `notificationSucceeded`
（`internal/usecase/notification_service.go:182`）判定条件是「2xx 且（空正文 或 `success` 不分大小写）」，
**已经天然兼容**。所以 Outbox、租约、指数退避（1/2/4/8/16/32 分钟）、死信全部可以原样复用，
只需换消息体与签名器。

**③ 商户不再只有一个**

现在密钥是 `settings.key` 单值，回调地址是订单上的 `notify_url`。多商户后：
每次回调都必须知道「用哪个商户的密钥签、发到哪个地址、按哪个版本签」。
这三件事必须跟着订单走，不能在下单后从全局配置读——否则商户改配置会让历史订单的回调签名错误。

---

## 2. 数据模型

### 2.1 新增 `merchants`

```sql
CREATE TABLE merchants (
  id            BIGINT NOT NULL AUTO_INCREMENT,
  pid           BIGINT NOT NULL,                    -- EPay 的 pid，对外暴露
  name          VARCHAR(128) NOT NULL,
  state         TINYINT NOT NULL DEFAULT 1,         -- 1 启用 / 0 停用
  -- 协议开关：决定这个商户能用哪套接口，可同时开启
  enable_v1     TINYINT(1) NOT NULL DEFAULT 1,
  enable_v2     TINYINT(1) NOT NULL DEFAULT 0,
  -- 回调与限额
  default_notify_url  TEXT NOT NULL,
  default_return_url  TEXT NOT NULL,
  -- 金额策略（见 §3.2）：0 严格等额 / 1 允许微调
  allow_price_adjust  TINYINT(1) NOT NULL DEFAULT 0,
  max_amount_cent     BIGINT NOT NULL DEFAULT 0,   -- 0 表示不限制
  remark              VARCHAR(255) NOT NULL DEFAULT '',
  created_at    DATETIME(6) NOT NULL,
  updated_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_merchants_pid (pid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

### 2.2 新增 `merchant_keys`（密钥版本化，支撑轮换）

```sql
CREATE TABLE merchant_keys (
  id            BIGINT NOT NULL AUTO_INCREMENT,
  merchant_id   BIGINT NOT NULL,
  protocol      TINYINT NOT NULL,                   -- 1=V1(MD5) 2=V2(RSA)
  version       INT NOT NULL,                       -- 同一商户同一协议内递增
  state         TINYINT NOT NULL DEFAULT 1,         -- 1=active(可用于签发) 0=retired(仅验签)
  -- V1：AES-GCM 密文；V2：平台私钥密文
  secret_cipher BLOB NOT NULL,
  secret_nonce  BLOB NOT NULL,
  -- V2 专用：商户公钥（公钥不敏感，明文存）
  peer_public_key TEXT NOT NULL DEFAULT '',
  -- 展示用后四位，避免每次都解密
  hint          VARCHAR(8) NOT NULL DEFAULT '',
  created_at    DATETIME(6) NOT NULL,
  retired_at    DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_merchant_keys_version (merchant_id, protocol, version),
  KEY idx_merchant_keys_lookup (merchant_id, protocol, state)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

设计要点：

- **多版本共存**：轮换时新建 `version+1` 并置 `active`，旧行转 `retired`。
  验签时按 `state DESC, version DESC` 遍历全部密钥逐一尝试——轮换窗口期内新旧请求都能通过，
  签发与回调签名只用 `active` 那条。
- **密钥不落明文**：`secret_cipher/nonce` 由 AES-256-GCM 加密，主密钥来自环境变量（见 §4.1）。
- **只展示一次**：明文仅在创建/轮换的响应里返回一次，此后接口只回 `hint`（后 4 位）。

### 2.3 改造 `orders`

```sql
ALTER TABLE orders
  ADD COLUMN merchant_id BIGINT NOT NULL DEFAULT 0 AFTER id,
  ADD COLUMN protocol TINYINT NOT NULL DEFAULT 0 AFTER merchant_id,  -- 0=VMQ原生 1=EPayV1 2=EPayV2
  ADD COLUMN epay_type VARCHAR(16) NOT NULL DEFAULT '' AFTER payment_type,
  ADD COLUMN merchant_order_name VARCHAR(128) NOT NULL DEFAULT '' AFTER epay_type;

-- pay_id 唯一性从全局收敛为「商户内唯一」
ALTER TABLE orders DROP INDEX uq_orders_pay_id;
ALTER TABLE orders ADD UNIQUE KEY uq_orders_merchant_pay_id (merchant_id, pay_id);
ALTER TABLE orders ADD KEY idx_orders_merchant_state (merchant_id, state, created_at);
```

- `merchant_id=0` 保留给 VMQ 原生接口（等价于「系统商户」），保证存量数据与现有集成零影响。
- `epay_type` 存原始字符串（`alipay`/`wxpay`），`payment_type` 存内部枚举，
  映射见 §5.2。两者都存是为了回调时能原样回显 `type`——EPay 要求回调的 `type` 与下单一致，
  而内部枚举可能随渠道扩展而变化。

### 2.4 迁移顺序（必须可回滚）

1. 建 `merchants`、`merchant_keys` 两张新表（无侵入）
2. `orders` 加列（`DEFAULT` 保证旧行可读）
3. 把 `settings.key` 迁移为一个 `pid=1` 的 V2 商户？**不迁移**。
   原生接口继续读 `settings.key`，EPay 商户从 `merchants` 读，两条路径互不干扰。
4. 最后再加 `(merchant_id, pay_id)` 唯一索引并删除旧索引

---

## 3. 核心难点

### 3.1 金额锁必须保持全局唯一

`price_locks` 主键是 `(payment_type, amount_cent)`，**不能加 `merchant_id`**。

原因：所有商户共用同一批收款码和同一个挂机端。挂机端只上报「微信到账 10.03 元」，
服务端据此在 `orders` 里找 `payment_type=1 AND really_price_cent=1003 AND state=0` 的待支付订单。
如果允许两个商户同时锁住 1003，这一笔到账将无法归属，必然产生资金错账——
这正是 V3.1 修复过的 P0 级问题。

**推论**：多商户不等于多资金池。多个商户共享一个金额空间，同一时刻同一金额只能有一个待支付订单。
这是免签架构的物理约束，不是实现选择，必须在产品说明里对商户讲清楚。

### 3.2 金额微调与 EPay 语义的冲突

商户传 `money=10.00`，若锁被占用，只有三条路：

| 方案 | 做法 | 优点 | 缺点 |
| :-- | :-- | :-- | :-- |
| **A 严格等额**（默认） | 锁不住就返回「金额被占用」，让商户/用户重试 | 回调 `money` 与实付完全一致，对账零歧义 | 高峰期失败率上升，用户需重试 |
| **B 允许微调** | 锁不住就 +0.01~0.99，回调仍回 `money=10.00` | 下单成功率高 | 用户实付 10.03，商户只知 10.00，差额需人工对账 |
| C 回调回真实金额 | 回调 `money=10.03` | 账面一致 | **商户金额校验失败，发货中断，不可用** |

**默认 A**，按商户开启 B（`merchants.allow_price_adjust`）。

方案 B 的关键约束：**回调签名域只能使用 EPay 标准字段**，`money` 必须回下单金额。
不能塞 `really_money` 之类自定义字段——MD5 验签是对「全部非空参数」排序拼接，
多一个字段商户侧必然验签失败。真实金额只落本地 `orders.really_price_cent`，供管理端对账。

> 这意味着 `settings.payQf`（现有全局微调配置）对 EPay 通道**默认不生效**，
> 必须由商户级别的 `allow_price_adjust` 显式开启。原生 VMQ 通道保持原行为。

### 3.3 挂机端与协议无关

挂机端只做两件事：心跳、推送「类型 + 金额 + 时间戳」。
它不感知订单来自原生接口还是 EPay，也不感知商户。
所以 **Android 端零改动**，EPay 兼容完全在服务端完成。这是本次设计能控制住工作量的根本原因。

---

## 4. 密钥管理

### 4.1 主密钥（Master Key）

```
VMQ_MASTER_KEY = base64(32 bytes)   # AES-256-GCM 主密钥
```

- 未配置时**拒绝启动**并给出生成命令：`openssl rand -base64 32`。
  静默降级为明文存储是不可接受的——密钥一旦进 DB 备份就等于泄露。
- 支持 `VMQ_MASTER_KEY_OLD` 做主密钥轮换：解密时先试新键再试旧键，重新加密后清空旧键。
- 主密钥只从环境变量读，**不进 `settings` 表**（否则等于把锁和钥匙放同一个盒子）。

### 4.2 V1 密钥（MD5 共享密钥）

生成规则：

```go
// 32 字符，crypto/rand，小写 base32（去掉易混淆字符 0/O/1/I/L）
const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
// 熵 ≈ 32 * log2(31) ≈ 158 bit，远超 MD5 碰撞与暴力破解需求
```

为什么不用 `uuid`：EPay 生态里密钥常被复制粘贴到各种面板，去掉易混淆字符能显著减少
「密钥抄错」这类工单。

存储：AES-GCM 加密 → `merchant_keys.secret_cipher/nonce`，`hint` 存末 4 位。

### 4.3 V2 密钥（RSA 双向）

EPay V2 是**双向签名**：

| 方向 | 签名方 | 验签方 | 用哪把钥匙 |
| :-- | :-- | :-- | :-- |
| 商户 → 本网关（下单/查询） | 商户 | 本网关 | 商户私钥签，**本网关持商户公钥验** |
| 本网关 → 商户（响应/回调） | 本网关 | 商户 | **本网关平台私钥签**，商户持平台公钥验 |

所以每个开 V2 的商户需要两组密钥：

1. **平台密钥对**：本网关生成，私钥加密入库（`merchant_keys.secret_cipher`），
   公钥通过管理接口导出给商户填进他们后台。
   > 严格说平台密钥对应全局唯一一把更合理，但商户各自一把能实现"单商户泄露不影响他人"，
   > 代价是商户换平台时要重配。这里选**每商户一把**，隔离性优先。
2. **商户公钥**：由商户在自己的 EPay 后台生成后粘贴进来，存 `merchant_keys.peer_public_key`（明文）。

生成：RSA 2048，PKCS#8 PEM。2048 位是 EPay 生态的通行值，4096 在部分老 PHP 环境验签会超时。

### 4.4 轮换流程

```
1. 管理端「轮换密钥」→ 生成 version+1，state=active
2. 旧 version 置 state=retired（不删）
3. 商户把新密钥填进自己的面板（V1 直接替换；V2 换平台公钥）
4. 观察 7 天，期间新旧密钥并行验签
5. 确认无流量后，手动清理 retired 行
```

不做自动清理：停用太早会让仍在用旧密钥的商户突然全部失败，且日志里只会看到"签名错误"。

---

## 5. 接口设计

### 5.1 路由

```
# EPay V1（与现有 /api 前缀完全隔离，避免与原生接口混淆）
GET|POST /submit.php          页面跳转支付
POST     /mapi.php            API 支付，返回 JSON
GET|POST /api.php             act=order（查单）/ act=query（查商户）

# EPay V2
POST     /api/pay/create      统一下单
POST     /api/pay/query       查询
POST     /api/pay/close       关单
POST     /api/merchant/info   商户信息

# 管理端（走现有 JWT 鉴权）
GET      /api/merchant/list
POST     /api/merchant/create
POST     /api/merchant/update/:id
POST     /api/merchant/key/rotate/:id
GET      /api/merchant/key/reveal/:id    # 仅创建后 10 分钟内可读一次明文
```

`submit.php` / `mapi.php` 必须挂在根路径而非 `/api` 下——大量 EPay 插件把
「网关地址 + `/submit.php`」硬编码拼接，改路径等于不可用。

### 5.2 参数映射

| EPay 参数 | 内部字段 | 说明 |
| :-- | :-- | :-- |
| `pid` | `merchants.pid` | 查商户，失败回 `code=-1 商户不存在` |
| `type` | `orders.epay_type` + `payment_type` | 见下表映射 |
| `out_trade_no` | `orders.pay_id` | 商户单号，`(merchant_id, pay_id)` 唯一 |
| `money` | `orders.price_cent` | 元，两位小数字符串，**保留原始文本** |
| `name` | `orders.merchant_order_name` | 商品名，仅存储与回显 |
| `notify_url` | `orders.notify_url` | 必须 http(s) |
| `return_url` | `orders.return_url` | 必须 http(s) |
| `sign` / `sign_type` | — | 验签用，不落库 |
| `device` | — | `pc`/`mobile`，仅影响收银台跳转形态 |
| `sitename`、`param` 等 | — | 参与验签但忽略内容（V1 签名覆盖全部非空参数） |

支付方式映射：

| EPay `type` | 内部 `payment.Type` | 当前可用性 |
| :-- | :-- | :-- |
| `alipay` | `payment.Alipay (2)` | ✅ 已有收款码 |
| `wxpay` | `payment.Wechat (1)` | ✅ 已有收款码 |
| `unionpay`、`bank`、`qqpay` | — | ❌ 返回 `code=-1 支付方式未开通` |

> 未开通的渠道必须**明确拒绝**，不能回落到微信/支付宝。挂机端只能监听微信/支付宝通知栏，
> 静默回落会让用户付了钱但类型不匹配，导致到账无法核销。

### 5.3 下单响应

**`mapi.php`（V1）**

```json
{
  "code": 1,
  "msg": "",
  "trade_no": "20261113000001",          // 本网关订单号
  "payurl": "https://host/#/payment/<publicToken>",
  "qrcode": "wxp://f2f0...",             // 收款码原始内容，商户可自行渲染
  "urlscheme": ""
}
```

`qrcode` 直接给收款码内容而非图片 URL，让商户用自己前端渲染二维码，省一次往返。
`payurl` 复用现有收银台（hash 路由 `#/payment/:publicToken`），自然继承支付宝转账确认单能力。

**`/api/pay/create`（V2）**

```json
{
  "code": 1,
  "msg": "",
  "trade_no": "20261113000001",
  "out_trade_no": "M20261113001",
  "payurl": "https://host/#/payment/<publicToken>",
  "qrcode": "wxp://f2f0...",
  "timestamp": "1697452800",
  "sign": "Base64...",
  "sign_type": "RSA"
}
```

V2 的响应必须带 `timestamp` 并参与签名，否则商户侧无法防御响应重放。

### 5.4 错误码

EPay 生态约定 `code=1` 成功，其余失败。沿用：

| 场景 | code | msg |
| :-- | :-- | :-- |
| 成功 | `1` | 空或 `success` |
| 签名错误 | `-1` | `签名错误` |
| 商户不存在/停用 | `-1` | `商户不存在` |
| 协议未开通 | `-1` | `该商户未开通 V1/V2 接口` |
| 支付方式未开通 | `-1` | `支付方式未开通` |
| 金额非法 | `-1` | `金额格式错误` |
| 金额被占用（严格等额） | `-1` | `该金额正在支付中，请稍后重试` |
| 订单号重复 | `-1` | `商户订单号已存在` |
| 监控端离线 | `-1` | `收款通道异常` |

HTTP 状态码统一 `200`，错误放在 `code`/`msg`——EPay 插件普遍只读 body，不看状态码。

---

## 6. 回调设计

### 6.1 V1 回调（GET）

```
GET {merchant.notify_url}?pid=1001&trade_no=20261113000001&out_trade_no=M20261113001
    &type=alipay&name=商品名&money=10.00&trade_status=TRADE_SUCCESS
    &sign=<md5>&sign_type=MD5
```

关键点：

- `money` 回**下单金额**（`orders.price_text`），不是真实到账金额（见 §3.2）
- 签名用**该商户的 V1 密钥**，不是全局 `settings.key`
- 期望响应 `success`，现有判定已兼容
- 用 GET 是因为 EPay 生态的绝大多数商户端只实现了 GET 接收；POST 会导致 404

### 6.2 V2 回调（POST form）

字段同 V1，额外 `timestamp`，`sign_type=RSA`，用**平台私钥**签名。
验签在商户侧用平台公钥。

### 6.3 与现有 Outbox 的衔接

`notification_outbox` 现有字段已够用，但消息体需要携带协议上下文：

```go
type epayNotifyPayload struct {
    Protocol   int    // 1=V1 2=V2
    MerchantID int64
    KeyVersion int    // 落库时快照，避免回调时用了轮换后的新密钥
    // ...EPay 标准字段
}
```

**`KeyVersion` 必须在下单/支付成功时就快照进消息体**。否则：商户轮换密钥后，
一条还没来得及投递的旧订单回调会用新密钥签名，商户按旧密钥验签必然失败。
这是密钥轮换最容易踩的坑。

`topic` 区分 `epay.v1.notify` / `epay.v2.notify`，便于按协议观察投递健康度。

---

## 7. 代码落位

```
internal/domain/epay/                 【新增】纯协议层，无外部依赖
  sign_v1.go                          V1 签名/验签（ASCII 排序 + MD5）
  sign_v2.go                          V2 签名/验签（SHA256WithRSA + timestamp）
  params.go                           参数常量、必填校验
  type_map.go                         EPay type 字符串 ↔ payment.Type
  errors.go                           错误码与文案
  *_test.go                           用官方示例报文做黄金用例

internal/domain/merchant/             【新增】实体
  merchant.go                         Merchant、Key、协议枚举、状态
  keygen.go                           V1 密钥生成、RSA 2048 密钥对生成

internal/domain/secret/               【新增】AES-256-GCM 封装
  secretbox.go                        Seal/Open，主密钥轮换双读

internal/port/ports.go                【改】加 MerchantRepository、MerchantKeyRepository
internal/adapter/mysql/merchant_repository.go   【新增】
internal/adapter/mysql/merchant_key_repository.go 【新增】

internal/usecase/merchant_service.go  【新增】创建/轮换/启停、明文只读一次
internal/usecase/epay_service.go      【新增】EPay 下单、查询、关单
  └─ 内部复用 OrderService 的 acquirePrice / resolvePayURL，不重复实现资金逻辑

internal/http/handler/epay_v1.go      【新增】submit.php / mapi.php / api.php
internal/http/handler/epay_v2.go      【新增】/api/pay/*
internal/http/handler/merchant.go     【新增】管理端 CRUD

internal/http/server.go               【改】挂载根路径路由
internal/app/app.go                   【改】装配新仓库与服务
internal/config/config.go             【改】读 VMQ_MASTER_KEY

database/migrations/                  【新增】分步 SQL，见 §2.4
web/src/views/merchants/index.tsx     【新增】商户管理页
```

**分层纪律**：`internal/domain/epay` 只做字符串与字节运算，不 import `database/sql`、
不 import `gin`、不碰 `context`。这样签名算法可以用官方报文做纯函数测试，
也避免协议细节渗进业务层。

---

## 8. 实施阶段

| 阶段 | 内容 | 验收 |
| :-- | :-- | :-- |
| **P0** | `merchants`/`merchant_keys` 建表、AES-GCM 封装、主密钥配置 | 单测覆盖加解密与主密钥轮换；未配主密钥时启动失败并给出提示 |
| **P1** | V1 签名/验签 + `mapi.php` + 回调 | 用 EPay 官方示例报文验签通过；与真实独角数卡跑通一笔小额单 |
| **P2** | `submit.php` + `api.php`（查单/查商户） | 浏览器跳转链路通；`act=order` 能查到已支付状态 |
| **P3** | V2 签名 + `/api/pay/create`、`query`、`close` | RSA 双向验签通过；`timestamp` 超窗被拒 |
| **P4** | 管理端商户 CRUD + 密钥轮换 | 明文只读一次；轮换后旧密钥仍可验签 |
| **P5** | 多商户隔离审计 | 跨商户查单/删单返回 404；`(merchant_id, pay_id)` 唯一生效 |

P1 之前不要动 `orders` 表结构——先把协议层用纯函数测通，再加字段，能显著降低调试成本。

---

## 9. 风险与取舍

| 风险 | 影响 | 应对 |
| :-- | :-- | :-- |
| 多商户共享金额空间 | 同额并发下单会失败 | 产品说明写明；提供 `allow_price_adjust` 开关；管理端展示「金额冲突率」指标 |
| 微调导致商户对账差 | 实收比订单多 0~0.99 元 | 默认关闭；开启时管理端订单详情并列展示 `price` 与 `reallyPrice` |
| V2 密钥配置门槛高 | 个人用户填不明白 RSA 公钥 | 管理端提供「一键生成平台密钥对 + 复制公钥」；V2 默认关闭，V1 默认开启 |
| 密钥轮换窗口期漏签 | 旧订单回调用新密钥 | 消息体快照 `KeyVersion`（§6.3） |
| 主密钥丢失 | 所有商户密钥不可解密 | 文档强调备份；提供 `vmqfox-api -rotate-master-key` 子命令 |
| 多商户弱化单商户假设 | 现有 `singleton_guard` 逻辑与新表并存 | 管理员仍唯一（管后台），商户是业务维度，两者不冲突 |

---

## 10. 确认状态

### 已确认（2026-11-13）

- **角色**：本网关扮演 EPay 平台侧，站点填「网关 / PID / 密钥」三格即可接入。
- **商户模型**：多商户，独立 pid + 密钥 + 回调地址。
- **协议版本**：EPay 官方 V1（MD5）与官方 V2（SHA256WithRSA）**都实现**。
  V2 门槛较高（站点需支持 RSA 插件），按 §8 排在 P3，V1 优先跑通并可独立上线。

### 仍需你拍板

1. **金额策略默认值**：我按「默认严格等额、商户可开微调」设计（§3.2 方案 A）。
   如果你预期并发量高、宁可让用户多付几分也要保证下单成功率，应改为默认开微调。

2. **`pid` 的形态**：EPay 生态里 `pid` 普遍是数字（如 `1001`）。
   本设计用 `BIGINT`。如果你希望用可读的字符串 ID（如 `vmq_abc123`），需要改成 `VARCHAR(32)`，
   并调整部分插件的兼容性预期。

3. **V2 的平台密钥粒度**：本文按「每个商户一把平台密钥对」设计（隔离性好，但商户换平台要重配）。
   另一种是全局一把平台密钥对（配置简单，但任一商户泄露会影响全部）。当前选前者。

---

## 附录 A：V1 签名黄金用例

```go
// 输入（注意 money 保留 "12.50" 而非 "12.5"）
params := map[string]string{
    "pid":          "1001",
    "type":         "alipay",
    "out_trade_no": "M20261113001",
    "notify_url":   "https://m.example.com/notify",
    "return_url":   "https://m.example.com/return",
    "name":         "测试商品",
    "money":        "12.50",
}
key := "abcdefghjkmnpqrstuvwxyz23456789"

// 待签串（ASCII 升序，剔除空值，末尾直接接 key）
// money=12.50&name=测试商品&notify_url=https://m.example.com/notify
// &out_trade_no=M20261113001&pid=1001&return_url=https://m.example.com/return
// &type=alipayabcdefghjkmnpqrstuvwxyz23456789

// sign = md5(上述字符串)，小写十六进制
```

注意 `type=alipay` 后面**紧接**密钥，没有 `&`。这是 V1 实现里最常见的错误点。

## 附录 B：与现有能力的复用关系

| 现有能力 | EPay 是否复用 | 说明 |
| :-- | :-- | :-- |
| `price_locks` 金额占锁 | ✅ 完全复用 | 全局唯一，不加商户维度 |
| `qrcodes` 选码 | ✅ 完全复用 | 含通用码 `price_cent=0` 回落 |
| `outbox` + 退避重试 | ✅ 完全复用 | `success` 判定已兼容 |
| `public_token` 收银台 | ✅ 完全复用 | `payurl` 指向同一页面 |
| 支付宝转账确认单 | ✅ 自动继承 | 收银台已支持，EPay 订单同样生效 |
| `settings.key` 原生验签 | ✅ 保留 | 与 EPay 商户密钥互不影响 |
| 挂机端协议 | ❌ 零改动 | 与协议无关 |
