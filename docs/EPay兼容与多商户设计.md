# EPay 兼容与多商户支持 · 设计方案

> 状态：待评审（v3.4，已按 2026-11-13 设计评审结论修订）
> 目标版本：V3.3
> 前置事实：本网关当前为**单商户**（`admin_credentials.singleton_guard` 在 DB 层强制唯一），
> 支付方式仅 `payment.Wechat=1` / `payment.Alipay=2`，到账识别依赖挂机端按**金额**全局匹配。
>
> **v3.4 修订摘要**（完整变更见附录 D）：
> ① 撤回「投递层零改动」，补 Outbox 载荷版本化与 topic 分派；
> ② 回调签名策略拆为「V1 入队固化 / V2 投递时重签」，解决与 V2 `timestamp` ±300s 的互斥；
> ③ 支付核销与回调载荷生成解耦事务，签名失败不得回滚支付事实；
> ④ 金额微调增加「精确金额码存在性」硬前提，并补结算与对账口径；
> ⑤ 补迁移机制（`schema_migrations` + `-migrate`）、回滚边界与两份 `schema.sql` 的同步要求；
> ⑥ 补同步回跳 `return_url`、下单幂等、`notify_url` 下单期预校验、EPay 端点限流；
> ⑦ 商户对外标识改为 `merchant_no`，自增 `id` 只做内部主键；
> ⑧ V2 锚定**彩虹/ezfp 系**（2026-11-14 定），P3 前只需核对三项细节（附录 C）。

---

## 0. 决策摘要

| 项 | 决策 |
| :-- | :-- |
| 协议角色 | 本网关**扮演 EPay 平台侧**，对外提供 `submit.php` / `mapi.php` / `api.php` 与 V2 的 `/api/pay/*` |
| 商户模型 | **多商户但站内自控**：内部自增 `id` 作主键，对外用随机 `merchant_no` 作 `pid`；只存名称/启停/备注，订单按商户隔离 |
| 商户范围 | **不开放自助注册**。商户由运营者在管理端手工创建，可以是运营者自己的多个站点，也可以是合作方站点（见 §0.3） |
| 协议版本 | **V1（MD5）先上线；V2（SHA256WithRSA）按彩虹/ezfp 系实现**：字段与 V1 同构、新增 `timestamp`、改用 `/api/pay/*`（见 §6.2、附录 C） |
| VMQ 原生接口 | **保留不动**，与 EPay 双协议并存 |
| 收款码 | 所有商户**共用同一批** `qrcodes`，这是免签系统的物理前提 |
| 金额锁 | `price_locks` 主键**保持全局唯一**，不引入商户维度（物理约束，见 §3.1） |
| 金额策略 | EPay 通道**默认开启微调**（+0~9 分，只增不减），且**候选金额必须存在精确码**，否则跳过；全局开关 `epayPriceAdjust` 可关（见 §3.2） |
| 密钥隔离 | **每商户每协议独立密钥**，V2 的平台 RSA 密钥对也按商户隔离，不共享（见 §4.3） |
| 回调投递 | **复用 Outbox 的存储/租约/退避/死信，投递层需扩展**：载荷版本化 + topic 分派 + 支持 GET；V1 签名入队固化，V2 投递时重签（见 §6.3） |
| 支付与通知 | **支付核销是事实，通知是可失败动作**：二者不共享事务边界（见 §6.6） |
| 升级路径 | 新增 `schema_migrations` + `-migrate`；`VMQ_MASTER_KEY` 仅在存在商户密钥时必填（见 §2.4、§4.1） |

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
| **本次新增 · EPay V2** | EPay 标准 SHA256WithRSA + `/api/pay/*` | 填三格 + 双方交换公钥 | 彩虹/ezfp 系新版站点 |

三者共用同一套订单、金额锁、收款码与挂机端，互不干扰。

### 0.2 术语澄清：三个「v2」不要混

本文档里出现的「V2」有三种，含义完全不同：

| 写法 | 指什么 | 签名 | 现状 |
| :-- | :-- | :-- | :-- |
| **EPay V1** | 本文新增，易支付旧版商户协议 | MD5，密钥拼在待签串末尾 | 待实现 |
| **EPay V2** | 本文新增，易支付新版商户协议 | SHA256WithRSA 双向 + `timestamp` | 待实现（按彩虹/ezfp 系） |
| **VMQ v2** | 仓库现有，`/api/order/create` 等用的协议 | HMAC-SHA-256，密钥作 HMAC key | 已在生产运行 |

`payment.SignVersion2`、`CreateSignV2`、Wiki 的「签名协议 v2」、以及报错文案
「检测到已废弃的 v1 签名」里的 v1/v2，**全部指第三行**，与 EPay 的协议版本无关。
新增代码中一律用 `EPayV1` / `EPayV2` / `VMQv2` 三个前缀显式命名，禁止裸写 `v1`/`v2`。

### 0.3 商户关系定义（本次新增，先钉死前提）

**本网关不是支付服务商，只是收款码的自动化核销工具。** 因此：

1. **钱不进网关账户**，直接进运营者上传的微信/支付宝收款码对应账户；网关只做「到账金额 → 订单」的匹配。
2. **网关不做商户结算、不做分账、不做余额**。运营者与合作方之间怎么分账，在网关之外完成。
3. 因此：**不开放商户自助注册**，`merchants` 由管理端手工创建；不做密钥自助找回（丢失即轮换）。
4. 但**必须做商户级配额与审计**——"合作方"不等于"自己"，一个配错的脚本可以打满共享金额空间（§3.1）。

这条前提决定了下面所有简化：不设结算表、不设余额、不设商户后台。
若将来要接公开注册商户，需要先补：结算口径、商户级限额、密钥自助、审计与对账导出，工作量约为本期的 2~3 倍。

---

## 1. 为什么不能简单加个路由

EPay 看起来只是「换一套参数名」，实际有三个不能绕的硬约束：

**① 签名算法完全不同，且不可调和**

| | VMQ v2（现状） | EPay V1 | EPay V2 |
| :-- | :-- | :-- | :-- |
| 算法 | HMAC-SHA-256 | MD5 | SHA256WithRSA |
| 密钥参与方式 | 作为 HMAC key，不进明文 | **直接拼在待签串末尾** | 私钥签名 / 公钥验签 |
| 参数顺序 | 固定顺序，不排序 | **按 key 的 ASCII 升序** | 同 V1 |
| 空值 | 保留 | **剔除空字符串** | 剔除空值 |
| 金额格式 | 两位小数规范化 | **用原始字符串，不得重新格式化** | 以锚定实现为准 |
| 重放防护 | 无 | 无 | `timestamp` ±300s |

V1 的待签串构造必须逐字节复刻，任何"顺手规范化"都会导致第三方验签失败：

```
1. 剔除 sign、sign_type（参数名精确匹配，大小写变体一律拒绝）
2. 剔除值为空串的参数（空串 ≠ "0"，"0"、"0.00" 必须保留）
3. ksort(SORT_STRING) 按 key 的 ASCII 升序
4. key=value 用 & 连接（不做 URL 编码、不去空格）
5. 末尾直接追加商户 key（无 & 、无 key= 前缀）
6. md5() 取小写十六进制
```

**必须拒绝的输入**（否则待签串会出现二义性，等于给签名伪造开口子）：

- 参数名或参数值中含 `&`（`a=1&b=2` 与 `a=1` 加 `b=2` 无法区分）；
- 参数名含 `=`；
- 同名参数出现两次（Go 的 `url.Values.Get` 取第一个、PHP 的 `$_GET` 取最后一个，
  两侧取不同值即可绕过金额校验——这是 V1 最容易被忽略的洞）；
- 参数值含 NUL（`0x00`），MySQL 与日志都会截断它；
- 参数值含 `\r` / `\n` 等 C0 控制字符——`return_url` 会进日志、也会被拼进 302 `Location`，
  控制字符是日志注入与响应头注入的基本面。

**签名与发送的编码关系**（v3.4 补）：**签名域永远用解码后的原始值**；
组装回调 URL 时才 `urlencode`。反过来（先编码再签）或对已编码值二次编码，
商户侧验签 100% 失败，而日志里只会显示「签名错误」。

**② 回调语义相同，但投递层必须改**

EPay 回调要求商户返回纯文本 `success`。现有 `notificationSucceeded`
（`internal/usecase/notification_service.go:182`）判定条件是「2xx 且（空正文 或 `success` 不分大小写）」，
**这一条已天然兼容**，可以保留。

但 **`deliver()` 本身不能原样复用**（v3.4 更正）：

- `:91-93` 有 topic 白名单，只认 `order.paid.notify`，新增 `epay.*.notify` 会被判「不支持的通知主题」；
- `:105-111` HTTP 方法与 `Content-Type` 硬编码为 POST + form，V1 需要 GET；
- `:101-103` 要求 `NewForm` 非空，V1 的参数在 URL 上、body 为空；
- **最危险的是换载荷结构**：若只放开 topic 而不做结构兼容，新载荷反序列化进旧
  `NotificationPayload`（只有 `orderId/notifyUrl/newForm`）后 `NotifyURL` 为空，
  `:98-100` 会 `return true` —— **回调没发出去，消息却被标记为已投递**。

所以正确表述是：**复用 Outbox 的存储、租约、指数退避与死信；`deliver()` 需扩展三处**（见 §6.3）。

**③ 商户不再只有一个**

现在密钥是 `settings.key` 单值，回调地址是订单上的 `notify_url`。多商户后：
每次回调都必须知道「用哪个商户的密钥签、发到哪个地址、按哪个版本签」。
这三件事必须跟着订单走，不能在下单后从全局配置读——否则商户改配置会让历史订单的回调签名错误。

---

## 2. 数据模型

### 2.1 新增 `merchants`（内部 id + 对外 merchant_no）

商户由站内统一控制、数量有限，不引入独立配置表、不存回调地址与限额模板——
EPay 下单时 `notify_url` / `return_url` 本来就是每次必传，存默认值只会产生「配置与实际不一致」。

```sql
CREATE TABLE merchants (
  id            BIGINT NOT NULL AUTO_INCREMENT,   -- 内部主键，不对外
  merchant_no   CHAR(16) CHARACTER SET ascii NOT NULL, -- 对外 pid；crypto/rand 生成，避免枚举
  name          VARCHAR(64) NOT NULL DEFAULT '',  -- 后台辨认用
  enabled       TINYINT(1) NOT NULL DEFAULT 1,
  close_minutes INT NOT NULL DEFAULT 30,          -- 订单有效期（分钟）；0 = 跟随全局 settings.close
  note          VARCHAR(255) NOT NULL DEFAULT '', -- 签约主体/联系人，运维用
  created_at    DATETIME(6) NOT NULL,
  updated_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_merchants_no (merchant_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

**订单有效期（v3.4 定）**：`close_minutes` 默认 **30 分钟**，与主流发卡站的支付窗口一致；
填 `0` 表示跟随全局 `settings.close`（默认 5 分钟）。

生效顺序：`orders.protocol != 0` 的订单取 `merchants.close_minutes`（0 则回落全局），
原生订单仍只看全局——原生通道行为完全不变。
生命周期任务关闭订单时按 `order.merchant_id` 关联商户表取有效期；
商户中途调整有效期只影响尚未关闭的订单判定，已关闭订单不追溯。

刻意不设的字段及原因：

| 去掉的字段 | 替代方式 |
| :-- | :-- |
| 自增 ID 直接当 pid | **改为对外 `merchant_no`**：自增值可枚举，且泄露商户规模 |
| `enable_v1` / `enable_v2` | **协议是否开通由「该协议有没有配密钥」自然决定**，避免「开关开着但没密钥」的不一致 |
| `default_notify_url` / `default_return_url` | EPay 下单必传，直接落订单表（**禁止回落全局设置**，见 §5.2） |
| `allow_price_adjust` | 上移为全局设置 `epayPriceAdjust`（见 §3.2） |
| `max_amount_cent` / 余额 | 本网关不经手资金，无余额概念（见 §0.3） |

> 停用（`enabled=0`）只影响**新下单**；在途订单的回调照常投递到完成或死信，
> 否则等于把已收的钱变成不发货物。

**停用与删除（v3.4 补）**：

| 动作 | 允许条件 | 连带处理 |
| :-- | :-- | :-- |
| 停用 | 随时 | 拒绝新下单；已建订单照常支付/回调；密钥仍保留 |
| 删除 | 该商户**无待支付订单**、且 `notification_outbox` 中无该商户未完成消息 | 级联删除 `merchant_keys`；`orders` 保留（`merchant_id` 变成悬空引用，仅供查账，不能再重签） |
| 强制删除 | 仅 `-force` 管理命令，需二次确认 | 先对其待支付订单执行关闭并释放 `price_locks`，再删除；未完成消息置 `dead` |

- `price_locks` 不含商户维度（§3.1），删除商户前必须按订单 `order_id` 逐笔
  `ReleaseByOrderID`，不能依赖 `ReleaseOrphaned`——后者只判断"订单是否存在"，看不到商户；
- 删除商户后该商户的历史订单**无法重签回调**（密钥已删除），这是不可逆操作，管理端需红字提示；
- 推荐默认走"停用不删除"，删除只作为清理测试数据的手段。

### 2.2 新增 `merchant_keys`（两代制，够轮换且不膨胀）

```sql
CREATE TABLE merchant_keys (
  merchant_id     BIGINT NOT NULL,
  protocol        TINYINT NOT NULL,                 -- 1=EPayV1(MD5) 2=EPayV2(RSA)
  generation      TINYINT NOT NULL,                 -- 1=当前（用于签发） 0=上一代（仅验签）
  key_version     SMALLINT NOT NULL DEFAULT 1,      -- 主密钥版本，用于轮换与自检
  -- V1：共享密钥密文；V2：平台私钥密文。均由 AES-256-GCM 加密（见 §4.1）
  secret_cipher   BLOB NOT NULL,
  secret_nonce    BLOB NOT NULL,
  -- V2 专用：商户公钥，公钥不敏感，明文存。与 generation 一一对应
  -- 必须是 VARCHAR 不能是 TEXT：MySQL 5.7 禁止 BLOB/TEXT 带 DEFAULT（ERROR 1101），
  -- 而 PKIX RSA-2048 PEM 约 451 字节、4096 约 800 字节，1024 足够。
  peer_public_key VARCHAR(1024) NOT NULL DEFAULT '',
  hint            VARCHAR(16) NOT NULL DEFAULT '',  -- V1 存密钥末 4 位；V2 存公钥指纹前 16 位
  last_used_at    DATETIME(6) NULL,                 -- 排查"某商户密钥是否真的在用"
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (merchant_id, protocol, generation)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

设计要点：

- **每商户每协议最多两行**。主键 `(merchant_id, protocol, generation)` 天然保证这一点。
- **轮换 = 一个事务里的三步**：删 `generation=0` → 把 `generation=1` 改成 `0` → 插入新的 `generation=1`；
  事务开头先 `SELECT ... FROM merchants WHERE id=? FOR UPDATE` 串行化，防止两个管理员并发轮换。
- **验签读两行、逐一尝试**，且 **V2 必须用该行自己的 `peer_public_key`**，
  不能用 `generation=1` 的公钥统一验旧签名。签发与回调签名只用 `generation=1`。
- **轮换平台密钥时 `peer_public_key` 必须原样带入新代**（见 §4.4）。否则商户入站请求会全部验签失败。
- **协议开通判定**：`SELECT 1 FROM merchant_keys WHERE merchant_id=? AND protocol=? AND generation=1`。
  查不到即返回「该商户未开通 V1/V2 接口」，无需额外的布尔开关。
- 两代制牺牲了「保留三代以上做长期灰度」的能力。对这个场景够用：
  商户数量有限，轮换可以配合通知一次性完成。

### 2.3 改造 `orders`

```sql
ALTER TABLE orders
  ADD COLUMN merchant_id BIGINT NOT NULL DEFAULT 0 AFTER id,
  ADD COLUMN protocol TINYINT NOT NULL DEFAULT 0 AFTER merchant_id,      -- 0=VMQ原生 1=EPayV1 2=EPayV2
  ADD COLUMN epay_type VARCHAR(16) NOT NULL DEFAULT '' AFTER payment_type,
  ADD COLUMN merchant_order_name VARCHAR(128) NOT NULL DEFAULT '' AFTER epay_type;

-- pay_id 唯一性从全局收敛为「商户内唯一」：生产环境用单条 ALTER 完成，避免无约束窗口
ALTER TABLE orders
  DROP INDEX uq_orders_pay_id,
  ADD UNIQUE KEY uq_orders_merchant_pay_id (merchant_id, pay_id),
  ADD KEY idx_orders_merchant_state (merchant_id, state, created_at);
```

- `merchant_id=0` 保留给 VMQ 原生接口（等价于「系统商户」），保证存量数据与现有集成零影响。
- `epay_type` 存原始字符串（`alipay`/`wxpay`），`payment_type` 存内部枚举，
  映射见 §5.2。两者都存是为了回调时能原样回显 `type`。
- `param` 复用现有列（`VARCHAR(255)`）承载 EPay 的 `param`，回调原样回显（v3.4 补）。
- **同步改造点（漏一处就出事故）**：

| 位置 | 改什么 | 漏改后果 |
| :-- | :-- | :-- |
| `internal/domain/order/order.go` | 实体加 4 字段 | 无法传递商户上下文 |
| `internal/adapter/mysql/models.go` | `OrderRow` 加列 + `ToDomain` + `orderToRow` | **`merchant_id` 恒写 0，编译与测试都不报错**，表现为回调签名全错、查单全 NotFound |
| `internal/adapter/mysql/order_repository.go` | 5 个按 `pay_id` 的方法加 `merchant_id` 条件 | 换索引后 `WHERE pay_id=? FOR UPDATE` 失去最左前缀 → **全表扫描 + 锁全表，下单全局串行化** |
| `internal/port/ports.go` | `FindByPayID*` 签名加 `merchantID` | 编译器不会提醒，调用点漏传即跨商户串号 |
| `internal/usecase/order_service.go:205` | 重复单号检查带商户 | 跨商户撞单号 |
| `internal/usecase/fakes_test.go` | stub 签名 | 5 个测试文件同时编译失败（`:255` 的编译期断言会立刻报出来） |

### 2.4 迁移顺序（必须可回滚，且必须承认哪里不可回滚）

**前提变更（v3.4）**：仓库当前**没有任何迁移机制**——`-init-db` 只在 `!HasSchema` 时
用嵌入的 `internal/adapter/mysql/schema.sql` 整库建表（`internal/adapter/mysql/schema.go:20-32`），
没有版本表，也没有 `database/migrations/`。因此本次必须自带迁移能力：

1. 新增 `schema_migrations(version INT PRIMARY KEY, name VARCHAR(128) NOT NULL, applied_at DATETIME(6) NOT NULL)`；
2. 新增 `vmqfox-api -migrate`（幂等、可重复执行；启动时不自动迁移，与现有「启动不建表」策略一致）；
3. 迁移脚本放 `database/migrations/`，按文件名序号执行；
   **注意**：MySQL 的 DDL 会隐式提交，无法把多条 DDL 包进一个事务。
   所以单个迁移文件必须可重入（`CREATE TABLE IF NOT EXISTS` / 先查 `information_schema`），
   版本记录在 DDL 全部成功后才写入；中途失败重跑不会报「表已存在」。DML 步骤仍放事务内；
4. **`database/schema.sql` 与 `internal/adapter/mysql/schema.sql` 必须同步修改**——
   前者是参考/容器初始化副本，后者才是 `-init-db` 真正执行的嵌入副本；只改一份会导致新装缺表。

执行顺序：

1. 建 `merchants`、`merchant_keys`（无侵入）
2. `orders` 加 4 列（**一条 ALTER**；MySQL 5.7 无 `ALGORITHM=INSTANT`，拆成 4 条会重建 4 次表）
3. 新增迁移后的查询层改造（§2.3 表格）**与第 4 步同一次上线**，不能跨版本
4. 索引切换（**单条 ALTER 同时 DROP + ADD**，语句级原子，无「无唯一约束」窗口）
5. **不迁移 `settings.key`**：原生接口继续读它，EPay 商户从 `merchants` 读，两条路径互不干扰

**回滚边界（诚实表述）**：

- 加列可回滚（`DROP COLUMN`），但已写入的 `merchant_id` 会丢失，历史 EPay 订单降级为系统商户，回调再也签不对；
- **索引切换不可回滚**：一旦两个商户出现相同 `out_trade_no`，重新加 `uq_orders_pay_id` 必然 1062。
  因此上线前必须全量查重，并准备人工改号预案。

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

**因此必须配配额（v3.4 补）**：金额空间是共享资源，10 个候选把单个金额档的上限压到 10 笔待支付。
所以新增：

1. 商户级待支付并发上限（`epayMaxPendingPerMerchant`，默认 50）与建单 QPS 上限
   （按 `merchant_id` 分桶，复用进程内固定窗口限流器，不必引入 Redis）；
2. 全局「金额冲突率」指标（`acquirePrice` 返回 `CodeOverloaded` 次数 / 建单尝试次数），
   在管理端展示——它是判断「要不要扩容候选数」的唯一依据。

### 3.2 金额微调与 EPay 语义的冲突

商户传 `money=10.00`，若锁被占用，只有三条路：

| 方案 | 做法 | 优点 | 缺点 |
| :-- | :-- | :-- | :-- |
| **A 允许微调**（默认） | 锁不住就按 +1 分递增重试，最多 9 次；回调仍回 `money=10.00` | 下单成功率高 | 实付可能多几分，需并列展示两个金额 |
| **B 严格等额**（可关） | 锁不住就返回「金额被占用」 | 回调金额与实付完全一致 | 高峰期失败率上升 |
| C 回调回真实金额 | 回调 `money=10.03` | 账面一致 | **商户金额校验失败，发货中断，不可用** |

**默认 A**，由全局设置 `epayPriceAdjust` 控制，默认值 `1`（开启）。

微调直接复用现有实现（`order.CandidateAmounts`，`internal/domain/order/order.go:130`，
`priceLockAttempts = 10`）：

```
候选序列 = [10.00, 10.01, ..., 10.09]
全部被占 → CodeOverloaded「订单超出负荷，请稍后重试」
```

**硬前提：候选金额必须存在精确金额码（v3.4 补）**。
`resolvePayURL` 的顺序是「精确金额固定码 → 通用码（`price_cent=0`）→ 系统兜底码」。
若微调到 `10.01` 而码库里没有 10.01 的固定码，会回落到**通用码并要求买家手动输金额**——
买家看着发卡站的 `10.00` 付款，这笔到账永远匹配不上 `10.01` 的待支付单，直接掉单。
因此微调候选必须满足：

1. 存在 `payment_type` 匹配、`state=0`、`price_cent=候选值` 的收款码；
2. 不存在则跳过该候选继续 +1 分；
3. 全部候选都不满足 → 返回 `CodeOverloaded`，**不允许回落到通用码**。

可用全局开关 `epayRequireExactCode`（默认 `1`）关闭此约束，但关闭后需自行承担掉单率。

两个关键约束：

1. **只递增、不递减**。原生通道的方向由 `payQf` 决定（`1`=增、`2`=减），
   但 EPay 通道**固定用 `order.Increase`**，不复用 `payQf`——
   递减会导致少收商户的钱，在第三方对账里无法解释；
   若共用配置，`payQf=2` 时会把 EPay 也带成递减。
2. **回调签名域只能用 EPay 标准字段**，`money` 必须回下单金额。
   不能塞 `really_money` 之类自定义字段——MD5 验签是对「全部非空参数」排序拼接，
   多一个字段商户侧必然验签失败。真实金额只落 `orders.really_price_cent`。

实际偏差上限是 **+0.09 元（9 分）**，不是通常以为的「0.01~0.99」——因为候选数固定为 10。
候选数可通过 `epayLockAttempts` 调整（默认 10，上限 100），偏差上限随之变化。

**结算与对账口径（v3.4 补，此前缺失）**：

- 网关不经手资金（§0.3），所有款项进运营者收款码对应账户；
- 因此「应结算给商户」的口径为 **`SUM(really_price_cent)`**（实收），
  与商户对账时**以实收为准**，微调产生的分差归商户，平台不吃差——这也是「只增不减」能成立的商业理由；
- 管理端与导出必须同时给 `price`（下单金额）与 `reallyPrice`（实收）两列，
  且 `Statistics` 需拆双口径：现有实现按 `SUM(price_cent)` 统计（`order_repository.go:116-120`），
  微调开启后这个数字与银行流水对不上。

> 用尽候选仍撞锁属于极端情况（同一秒内同类型 10 笔同额订单），
> 此时返回失败比继续放大偏差更合理：偏差越大，商户对账越难解释。

### 3.3 挂机端与协议无关

挂机端只做两件事：心跳、推送「类型 + 金额 + 时间戳」。
它不感知订单来自原生接口还是 EPay，也不感知商户。
所以 **Android 端零改动**，EPay 兼容完全在服务端完成。这是本次设计能控制住工作量的根本原因。

> 注意：Android 零改动 ≠ 监控服务零改动。`markPaid` 需要按订单所属商户选择密钥与协议（§6.3）。

### 3.4 新增配置项一览（v3.4 整理）

运行时开关进 `settings` 表（管理端可改），部署相关进环境变量（改了要重启）：

| 配置项 | 存储 | 默认 | 作用 |
| :-- | :-- | :-- | :-- |
| `epayPriceAdjust` | settings | `1` | EPay 通道是否允许金额微调 |
| `epayRequireExactCode` | settings | `1` | 微调候选必须存在精确金额码，否则跳过 |
| `epayLockAttempts` | settings | `10` | 候选金额个数（上限 100），决定偏差上限 `n-1` 分 |
| `epayMaxPendingPerMerchant` | settings | `50` | 单商户待支付订单并发上限，防单商户吃满金额空间 |
| `epayCreateRateLimit` / `epayCreateRateWindow` | settings | `10` / `1m` | 商户维度建单限流（按 `merchant_id` 分桶） |
| `masterKeyFingerprint` | settings | 空 | 主密钥自检指纹，非敏感，仅用于启动比对 |
| `VMQ_MASTER_KEY` | env | 空 | AES-256-GCM 主密钥；存在商户密钥时必填 |
| `VMQ_MASTER_KEY_VERSION` | env | `1` | 当前主密钥版本号，写入密文的 `key_version` 列 |
| `VMQ_MASTER_KEY_OLD` | env | 空 | 轮换期旧键，重加密完成后清空 |
| `VMQ_MASTER_KEY_OLD_VERSION` | env | 当前版本-1 | 旧键版本号 |

---

## 4. 密钥管理

### 4.1 主密钥（Master Key）

```
VMQ_MASTER_KEY = base64(32 bytes)   # AES-256-GCM 主密钥
```

- **条件必填（v3.4 改）**：`merchant_keys` 为空或表不存在时，缺失只打 WARN 并正常启动；
  一旦需要加解密（创建商户、轮换、EPay 下单）而主密钥缺失，返回明确错误并附生成命令
  `openssl rand -base64 32`。
  理由：存量部署升级到本期时，新能力为零，却要求先备份改 env，属于「零收益 breaking change」；
  而存量部署若因此起不来，代价远大于收益。真正的硬失败点放在「第一次创建商户」。
- **主密钥配错比缺失更危险**：配置了错误的 32 字节同样能启动，之后所有解密失败，
  而 `hint` 照常显示，界面看起来一切正常。因此启动时做**指纹自检**：
  库中存 `master_key_fingerprint = HMAC-SHA256(master_key, "vmqfox/master-key-check/v1")[:8]`，
  不一致直接拒绝启动并提示「主密钥与数据库不匹配，请恢复原主密钥或重置全部商户密钥」。
- **AAD 绑定（v3.4 补，v3.5 修正）**：`Seal/Open` 必须带附加认证数据
  `merchant_id ‖ 0x00 ‖ protocol ‖ 0x00 ‖ purpose`，`purpose` 取 `epay.v1.key` / `epay.v2.private`。
  否则仓库查询漏了 `protocol` 条件时，会把 V2 平台私钥密文当 V1 共享密钥解开（GCM 会成功），
  然后拿一段 PEM 当 MD5 key 用，签名永远对不上、日志只显示「签名错误」。
  **AAD 里不能包含 `generation`**：轮换时 `generation=1` 会被 UPDATE 成 0，而密文不变，
  一旦 AAD 绑了 generation，旧代密文立刻认证失败——「上一代仅用于验签」会退化成「上一代一律解不开」，
  且日志只显示签名错误。generation 不属于安全边界（两代都是同一商户同一协议），不该进 AAD。
- **nonce**：每次 `Seal` 用 `crypto/rand` 取 12 字节随机数并与密文一起存（`secret_nonce`）；
  **禁止**任何确定性派生；`rand.Read` 失败必须返回错误，不许退化为零 nonce。
- **`key_version`**：密文行记录加密时使用的主密钥版本；解密时按版本选 key。
  版本未知（例如从更早版本恢复了一个更新的库）必须返回专用错误
  `ErrUnknownKeyVersion`，对外文案是「商户密钥无法解密，请联系管理员重新下发」，
  **不能**落到「签名错误」——后者会把运维引向商户配置。
- **指纹存放位置**：`settings` 表新增 `masterKeyFingerprint`（非敏感，仅用于自检比对），
  与 `VMQ_MASTER_KEY` 计算值不一致时拒绝启动。
- **主密钥轮换任务**（`-rotate-master-key`）：扫描 `merchant_keys` 中 `key_version != 当前版本` 的行，
  用旧键解密、新键重加密、更新 `key_version`；分批提交、可中断续跑、幂等；
  结束时输出「仍用旧版本加密的行数」，非 0 则以非 0 退出码结束，并拒绝清除 `VMQ_MASTER_KEY_OLD`。
- **密钥重置兜底**：主密钥彻底丢失时，唯一可行路径是"重置全部商户密钥"
  （重新生成 V1 密钥 / 重新导出 V2 平台公钥），所有商户需重新配置。
  该命令必须要求二次确认并打印受影响商户数，不允许静默执行。
- 支持 `VMQ_MASTER_KEY_OLD` 做主密钥轮换：解密先试新键再试旧键，重加密后清空旧键；
  重加密任务结束若仍有旧键加密行，**拒绝**清除 `VMQ_MASTER_KEY_OLD`。
  版本号由 `VMQ_MASTER_KEY_VERSION`（默认 1）与 `VMQ_MASTER_KEY_OLD_VERSION`（默认当前版本-1）指定，
  写入密文时用当前版本，读取时按行内 `key_version` 选键——不靠"试哪把能解开"来判断。
- 主密钥只从环境变量读，**不进 `settings` 表**（否则等于把锁和钥匙放同一个盒子）。
  Docker 部署注意：`docker-compose.yml` 是显式环境变量白名单，必须补 `VMQ_MASTER_KEY` 条目。

### 4.2 V1 密钥（MD5 共享密钥）

生成规则：

```go
// 32 字符，crypto/rand，小写 base32（去掉易混淆字符 0/O/1/I/L）
const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
// 熵 ≈ 32 * log2(31) ≈ 158 bit，远超 MD5 碰撞与暴力破解需求
```

为什么不用 `uuid`：EPay 生态里密钥常被复制粘贴到各种面板，去掉易混淆字符能显著减少「密钥抄错」这类工单。

> 注意区分：上面的 `alphabet` 是**31 个符号的字符集**，密钥要从中取 **32 个字符**。
> 不要把 alphabet 本身当密钥示例——附录 A 的示例 key 是 32 位，比 alphabet 多一个字符。

存储：AES-GCM 加密 → `merchant_keys.secret_cipher/nonce`，`hint` 存末 4 位，
解密后必须断言长度为 32 且字符全在 alphabet 内（任何编码/截断 bug 在解密点就 fail-fast）。

### 4.3 V2 密钥（RSA 双向）

EPay V2 是**双向签名**：

| 方向 | 签名方 | 验签方 | 用哪把钥匙 |
| :-- | :-- | :-- | :-- |
| 商户 → 本网关（下单/查询） | 商户 | 本网关 | 商户私钥签，**本网关持商户公钥验** |
| 本网关 → 商户（响应/回调） | 本网关 | 商户 | **本网关平台私钥签**，商户持平台公钥验 |

所以每个开 V2 的商户需要两组密钥：

1. **平台密钥对**：本网关生成，私钥加密入库（`secret_cipher`），公钥导出给商户。
   **每商户一把，不共享**——单商户泄露不会波及其他人。
2. **商户公钥**：商户在自己的后台生成后粘贴进来，存 `peer_public_key`（明文）。

生成：RSA 2048，PKCS#8 PEM。2048 位是 EPay 生态的通行值，4096 在部分老 PHP 环境验签会超时。

> **契约**：`peer_public_key` 属于「商户身份」，与 `generation` 一一对应，
> **不随平台密钥轮换而改变**。轮换只换我方私钥，商户公钥原样复制到新代。

### 4.4 轮换流程（两代制）

**整个事务先锁商户行**（`SELECT ... FROM merchants WHERE id=? FOR UPDATE`），避免并发轮换互相踩：

```sql
-- 0) 串行化 + 取旧公钥
SELECT peer_public_key FROM merchant_keys
 WHERE merchant_id=? AND protocol=? AND generation=1 FOR UPDATE;

-- 1) 丢弃更早的一代
DELETE FROM merchant_keys
 WHERE merchant_id = ? AND protocol = ? AND generation = 0;
-- 2) 当前代降级为「仅验签」
UPDATE merchant_keys SET generation = 0
 WHERE merchant_id = ? AND protocol = ? AND generation = 1;
-- 3) 写入新一代（peer_public_key 必须带入，不能留空）
INSERT INTO merchant_keys
  (merchant_id, protocol, generation, key_version, secret_cipher, secret_nonce, peer_public_key, hint, created_at)
VALUES (?, ?, 1, ?, ?, ?, ?, ?, NOW(6));
```

之后：

1. 旧代立刻只用于验签，新代负责签发与回调签名，**在途请求不受影响**；
2. 商户把新密钥填进自己的面板（V1 直接替换；V2 换成新的平台公钥）；
3. 下次轮换时第 1 步会自然清掉旧代，**不需要单独的清理任务**。

硬约束：

- **明文只在事务提交成功后才生成**并返回给管理员，且只返回一次；提交前生成会出现
  「管理员拿到一把从未入库的密钥」的灾难；
- 并发冲突（1062）必须映射为业务错误「轮换冲突，请重试」，不能是 500；
- 不做自动过期：停用太早会让仍在用旧密钥的商户突然全部失败，日志里只会看到「签名错误」；
- 轮换/导出必须写审计日志（操作者、目标商户、协议、新旧 `hint`、时间、来源 IP），**不记密钥**。

---

## 5. 接口设计

### 5.1 路由

```
# EPay V1（与现有 /api 前缀完全隔离，避免与原生接口混淆）
GET|POST /submit.php          页面跳转支付
POST     /mapi.php            API 支付，返回 JSON
GET|POST /api.php             act=order（查单）；其余 act 明确不实现

# EPay V2（彩虹/ezfp 系，见附录 C）
POST     /api/pay/create      统一下单
POST     /api/pay/query       查询
POST     /api/pay/close       关单
POST     /api/merchant/info   商户信息

# 管理端（走现有 JWT 鉴权）
GET      /api/merchant/list
POST     /api/merchant/create
POST     /api/merchant/update/:id
POST     /api/merchant/key/rotate/:id
GET      /api/merchant/key/public/:id      # 只导出平台公钥（V2）
```

`submit.php` / `mapi.php` 必须挂在根路径而非 `/api` 下——大量 EPay 插件把
「网关地址 + `/submit.php`」硬编码拼接，改路径等于不可用。

**`act` 白名单（v3.4 收窄）**：只实现 `order`（单笔查单）。
`act=query`（查商户）、`act=settle`（结算记录）、`act=orders`（批量）、`act=refund`（退款）
**一律返回 `code=-1 接口未开通`**：

- 本网关不经手资金，没有余额/结算/结算账号实体，返回假数据会误导商户（很多发卡站会先查余额再下单）；
- `act=query` 的原版实现会在响应里**回显商户密钥**，属主动泄露面。

**公开端点必须挂限流与写保护**：`submit.php`/`mapi.php` 等写接口按
「IP + `merchant_no`」双维度限流，并挂 `middleware.WriteGuardAlways`（与现有公开端点一致），
否则 shadow（只读）模式下 EPay 仍能写库。

### 5.2 参数映射

| EPay 参数 | 内部字段 | 说明 |
| :-- | :-- | :-- |
| `pid` | `merchants.merchant_no` | 查商户，失败回 `code=-1 商户不存在` |
| `type` | `orders.epay_type` + `payment_type` | 见下表映射；`mapi.php` 中为必填 |
| `out_trade_no` | `orders.pay_id` | 商户单号，`(merchant_id, pay_id)` 唯一 |
| `money` | `orders.price_cent` + `price_text` | 元，最多两位小数，**保留原始文本**（`"10"` 与 `"10.00"` 都要能回显） |
| `name` | `orders.merchant_order_name` | 商品名，仅存储与回显；超过 127 字节按规范截断 |
| `notify_url` | `orders.notify_url` | **必填**，http(s)，下单期预校验（§6.5）；**禁止回落全局设置** |
| `return_url` | `orders.return_url` | 可选；若为空则回跳时返回平台默认页 |
| `param` | `orders.param` | **落库并在回调原样回显**（EPay 规范要求） |
| `sign` / `sign_type` | — | 验签用，不落库 |
| `device` / `clientip` | — | 参与验签，内容忽略 |
| `sitename` | — | 参与验签，内容忽略 |

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
  "trade_no": "20261113000001",
  "payurl": "https://host/#/payment/<publicToken>",
  "qrcode": "wxp://f2f0...",
  "urlscheme": ""
}
```

`qrcode` 直接给收款码内容而非图片 URL，让商户用自己前端渲染二维码，省一次往返。
`payurl` 复用现有收银台（hash 路由 `#/payment/:publicToken`），自然继承支付宝转账确认单能力。

**`api.php?act=order`（V1 查单，v3.4 补字段表）**

```json
{
  "code": 1,
  "pid": "M7K3QPX9Z2AB4C",
  "trade_no": "20261113000001",
  "out_trade_no": "M20261113001",
  "type": "alipay",
  "name": "测试商品",
  "money": "10.00",
  "trade_status": "TRADE_SUCCESS",
  "addtime": "2026-11-13 12:00:00",
  "endtime": "2026-11-13 12:01:03",
  "param": ""
}
```

`trade_status` 只有两个值：`TRADE_SUCCESS`（已支付，含 `status=2` 通知失败）与
`WAIT_BUYER_PAY`（未支付/已关闭）。**未实现 `act=query/refund/settle/orders`**。

**V2 响应（按彩虹/ezfp 系）**：字段与 V1 同构（`pid` + `money` 元 + `code=1`），
额外返回 `timestamp` 且**参与签名**（否则商户侧无法防响应重放），`sign_type=RSA`。
即与 V1 的差异只有三处：签名算法、`timestamp`、接口地址。联调首日仍需核对三项细节，见附录 C。

### 5.4 幂等与错误码

**幂等（v3.4 补）**：EPay 的 `submit.php`/`mapi.php` 在下单超时、浏览器刷新、插件重试时
会用同一 `out_trade_no` 再次请求。必须幂等，否则会出现「商户认为下单失败、用户却已付款」。

规则：

| 场景 | 行为 |
| :-- | :-- |
| 同 `(merchant_id, out_trade_no)` 且 `price_cent`、`payment_type` 一致，状态为待支付 | **返回原订单**的 `trade_no`/`payurl`/`qrcode`，`code=1` |
| 同上，状态为已支付/通知失败 | 同样返回原 `payurl`（无害，且避免插件状态机错乱） |
| 同上，状态为已关闭 | 返回 `code=-1 订单已关闭，请重新下单` |
| 金额或支付方式不一致 | `code=-1 商户订单号已存在且金额不一致`（绝不静默改价） |

**实现约束**：幂等命中时**不得重新 `acquirePrice`**（`price_locks` 有 `uq_price_locks_order_id`，
会撞自己的锁并推进到下一个候选金额，导致订单金额漂移）。

错误码沿用 EPay 约定 `code=1` 成功，其余失败：

| 场景 | code | msg |
| :-- | :-- | :-- |
| 成功 | `1` | 空或 `success` |
| 签名错误 | `-1` | `签名错误` |
| 商户不存在/停用 | `-1` | `商户不存在` |
| 协议未开通 | `-1` | `该商户未开通 V1/V2 接口` |
| 支付方式未开通 | `-1` | `支付方式未开通` |
| 接口未实现（query/refund/settle） | `-1` | `接口未开通` |
| 金额非法 | `-1` | `金额格式错误` |
| 金额被占用（严格等额） | `-1` | `该金额正在支付中，请稍后重试` |
| 订单号重复且金额不一致 | `-1` | `商户订单号已存在且金额不一致` |
| 回调地址不合规 | `-1` | `notify_url 必须是公网可访问地址` |
| 监控端离线 | `-1` | `收款通道异常` |

HTTP 状态码统一 `200`，错误放在 `code`/`msg`——EPay 插件普遍只读 body，不看状态码。

### 5.5 handler 层必做校验（协议层管不到，漏一条就是一个洞）

| # | 检查点 | 做法 | 不做会怎样 |
| :-- | :-- | :-- | :-- |
| 1 | 同名参数重复 | 出现 ≥2 次直接拒绝 | Go 的 `url.Values.Get` 取第一个、PHP 的 `$_GET` 取最后一个，两侧取不同值即可绕过金额校验 |
| 2 | `sign`/`sign_type` 大小写变体 | `Sign`/`SIGN` 一律拒绝 | 我方剔小写、对方剔大写，签名域不一致 |
| 3 | 编码边界 | 验签用**解码后**的原值；组装发送时才 `urlencode`，且只编一次 | 先编码再签 / 二次编码，商户侧必失败且只报「签名错误」 |
| 4 | `money` 原文 | 落 `price_text` 并原样回显；`NormalizeAmount` 只用于我方生成回调 | 把 `12.5` 改写成 `12.50` 会直接改变签名域 |
| 5 | 回调地址 | `notify_url` 必填 + 下单期预校验（§6.5）；`return_url` 为空走默认成功页 | 空值回落全局设置会把回调发到别的商户 |
| 6 | V2 时间戳 | 先 `TimestampValid` 再验签；**禁止**先 `AppendTimestamp` 覆盖对方时间 | 等于用我方时间替换对方时间，重放防护失效 |
| 7 | V2 重签 | 响应与回调都在投递时重签 | 固定签名在 +7 分钟后必然过期（§6.2） |
| 8 | 未开通渠道 | 明确回「支付方式未开通」，不回落微信/支付宝 | 用户付了钱、类型不匹配，到账无法核销 |
| 9 | 幂等命中 | 不得重新 `acquirePrice` | `price_locks` 撞自己的锁，金额漂移 |
| 10 | 输入上限 | 请求体 ≤64KB、参数 ≤32 个 | 签名与歧义校验都是 O(参数字节)，等于开放 CPU 放大 |
| 11 | 歧义参数文案 | 对外统一「签名错误」（与真正验签失败**同码同文案同耗时**）；审计单独记「参数含歧义分隔符或控制字符」；对外文案固定为：「参数值包含不支持的字符（`&` 或控制字符），请将业务数据放入 `param` 并做 URL 编码，或改用路径段承载；商品名请去掉换行与制表符」 | 线上会被误诊成密钥配错，永远查不出来；文案不到位会变成反复工单 |
| 12 | 成功响应 | `code=1` 时 `msg` 固定空串（全站一致） | EPay 插件有的判空、有的判 `success`，不一致会随机失败 |

---

## 6. 回调设计

### 6.1 V1 回调（GET）

```
GET {order.notify_url}?pid=M2026...&trade_no=20261113000001&out_trade_no=M20261113001
    &type=alipay&name=%E5%95%86%E5%93%81%E5%90%8D&money=10.00&trade_status=TRADE_SUCCESS
    &param=&sign=<md5>&sign_type=MD5
```

关键点：

- `money` 回**下单金额**（`orders.price_text`），不是真实到账金额（见 §3.2）
- 签名用**该商户的 V1 密钥**，不是全局 `settings.key`
- **签名域取解码后的原始值**；发送时参数值 `urlencode`（空格用 `%20`，与 PHP `urlencode` 的 `+`
  必须二选一并写死，由联调验证）
- 期望响应 `success`，现有判定已兼容
- 用 GET 是因为 EPay 生态的绝大多数商户端只实现了 GET 接收；POST 会导致 404

### 6.2 V2 回调（POST form）

字段与 V1 一致（`pid/trade_no/out_trade_no/type/name/money/trade_status/param`），
额外 `timestamp`，`sign_type=RSA`，用**平台私钥**签名，商户用平台公钥验签。

> 少数分支把支付方式字段命名为 `channel`。本网关回调统一发 `type`；
> 若目标商户端只认 `channel`，则两个字段同值一起发（`type` 与 `channel` 都进签名域）。

**V2 的回调不能沿用「入队固化签名」**：响应与通知里的 `timestamp` 要参与签名，
而商户侧按 ±300s 校验时效；Outbox 的重试退避是 1/2/4/8/16/32 分钟、最多 20 次，
第 4 次（累计约 7 分钟）起签名必然过期。因此 V2 走 §6.3 的「存参数、投递时重签」。

### 6.3 与现有 Outbox 的衔接（v3.4 重写）

**结论：复用 Outbox 的存储、租约、退避、死信；投递层需扩展三处。**

```go
const (
    topicOrderPaid = "order.paid.notify"   // 原生 VMQ
    topicEPayV1    = "epay.v1.notify"
    topicEPayV2    = "epay.v2.notify"
)

type notifyPayload struct {
    Version   int    `json:"v"`            // 1=旧结构（仅 orderId/notifyUrl/newForm）
    OrderID   string `json:"orderId"`
    NotifyURL string `json:"notifyUrl"`
    // 旧字段：VMQ 原生与 EPay V1 使用
    NewForm   string `json:"newForm,omitempty"`
    // 新字段：EPay V1 用 Method+URL，V2 用 Method+URL+Body
    Method    string `json:"method,omitempty"` // GET / POST
    URL       string `json:"url,omitempty"`    // 已带签名参数的完整地址（V1）
    Body      string `json:"body,omitempty"`   // V2 待签字段的 JSON 快照，投递时重签
}
```

投递侧改造（`notification_service.go`）：

1. topic 分派：三个 topic 各自处理；**未知 topic 直接 `MarkDead`（终态），不得走 `settleFailed` 去改订单状态**；
2. 兼容旧载荷：`Version` 缺失或 `NewForm` 非空时走原 POST 路径——**库中已存在的消息必须能继续投递**；
   生产端必须**始终写 `notifyUrl`**（即使改用 `url`/`body`），否则旧版 worker 会误判为「无需通知」而静默完成；
3. `Method` 必须落在 `{GET, POST}` 白名单；GET 时不设 `Content-Type`、body 为空；
4. `NewForm` 非空校验改为「`URL` 或 `Body` 至少一个非空」，避免 V1 被误判；
5. **当 `notifyUrl` 非空、但 `url`/`newForm`/`body` 全空时必须判失败**——当前 `:98-100`（`NotifyURL == ""` 直接 `return true`）
   与 `:101-103`（只校验 `NewForm`）组合起来，正好形成「回调没发出去、消息却标记已投递」的静默路径。

**签名策略（v3.4 分协议）**：

| 协议 | 入队内容 | 投递时 | 理由 |
| :-- | :-- | :-- | :-- |
| VMQ 原生 | 完整 form + sign | 原样发 | 现状不变 |
| **EPay V1** | 完整 URL（含 sign）+ method=GET | **原样发** | 无时效字段，固化可避免轮换导致历史订单签错 |
| **EPay V2** | 字段 JSON 快照（不含 sign）+ method=POST | **用 `generation=1` 私钥重签并补 `timestamp`** | `timestamp` 有时效，固化必然过期 |

V2 重签要求投递 worker 能按 `order.merchant_id + protocol` 取密钥并解密，属于新增依赖，
但只让 V2 承担；V1 的「与密钥表解耦」收益保持不变。

`topic` 区分三值后，outbox 的唯一键 `(event_key, topic)` 天然隔离，互不顶替。

### 6.4 同步回跳 `return_url`（v3.4 补，此前整块缺失）

收银台支付成功页会调用 `GET /api/order/return-url/:publicToken` 取回跳地址
（`web/src/views/payment/result.tsx`）。现有实现用 `settings.key` + VMQ v2 字段签名，
**EPay 订单照此会验签失败**——用户付了钱、跳回商户站却显示「支付失败」。

必须按 `orders.protocol` 分支：

| protocol | 签名密钥 | 参数集 |
| :-- | :-- | :-- |
| 0（原生） | `settings.key` | `payId/param/type/price/reallyPrice` + `sign`（HMAC-SHA-256） |
| 1（EPayV1） | 该商户 V1 密钥 | `pid/trade_no/out_trade_no/type/name/money/trade_status` + `sign/sign_type=MD5` |
| 2（EPayV2） | 该商户平台私钥 | EPay V2 字段集 + `timestamp` + `sign/sign_type=RSA` |

约束：回跳只在订单为已支付/通知失败时生成；签名字段与 notify 共用同一份构造器，避免两套逻辑漂移。

### 6.5 `notify_url` 下单期预校验（v3.4 补）

出站 `notifier` 默认只允许 https、拒绝回环/私网/保留网段（`VMQ_NOTIFY_ALLOW_HTTP=false`、
`VMQ_NOTIFY_ALLOW_CIDR` 默认空）。若不预校验，会让「下单成功、付款成功、回调 100% 失败」
静默发生，且在 20 次退避（约 8 小时）后才死信。

因此下单时用**同一份出站策略**做只读预校验（不发起 HTTP）：

1. scheme 按 `VMQ_NOTIFY_ALLOW_HTTP` 判定；
2. 解析域名得到的每个 IP 跑 `checkIPSecurity`，命中私网/回环/元数据地址即拒绝；
3. 拒绝带 userinfo、IP 字面量、非常规端口（443/80 之外需显式放行）；
4. 校验失败返回 `code=-1 notify_url 必须是公网可访问地址`，**不写订单、不占金额锁**。

> 同时修正出厂配置：`env.example` 的 `VMQ_NOTIFY_ALLOW_CIDR=127.0.0.1/32` 应改为空，
> 并在注释中说明「仅同机部署 epay 时才显式放开，放开即等于允许向本机任意端口发起请求」。

### 6.6 支付核销与通知解耦（v3.4 补）

现状 `markPaid` 在 `Push` 的事务里生成通知载荷（`monitor_service.go:257-290`）。
EPay 引入了两个原生没有的失败源——AES-GCM 解密与 RSA 签名——一旦失败：

- 订单状态回滚为待支付，`price_locks` 不释放（该金额被占死）；
- `payment_events` 的 `event_key` 也一起回滚，**去重失效**，挂机端重推会被当作新事件再次失败；
- 结果是「钱已到账、系统里没有这笔支付」。

规则改为：

1. **状态变更、释放金额锁、写到账事件必须提交**；
2. 载荷生成放在事务提交之后（或独立短事务）；失败时订单落 `StatusNotifyFailed`，
   并写一条 `status=dead` 的 outbox 记录（`last_error` 记原因）供管理端重签；
3. 密钥读取/解密在事务外完成（现有实现也是事务外读 `settings.key`）；
4. 重签入口复用现有 `POST /api/order/reissue/:id`，但必须按 `protocol` 选择签名器。

---

## 7. 代码落位

```
internal/domain/epay/                 【新增】纯协议层，无外部依赖
  sign_v1.go                          V1 签名/验签（ASCII 排序 + MD5 + 输入二义性校验）
  sign_v2.go                          V2 签名/验签（SHA256WithRSA + timestamp）
  params.go                           参数常量、必填校验、长度与字符集限制
  type_map.go                         EPay type 字符串 ↔ payment.Type
  errors.go                           错误码与文案
  *_test.go                           官方示例报文 + 回调签发黄金用例

internal/domain/merchant/             【新增】实体
  merchant.go                         Merchant（含 merchant_no）、Key、协议枚举、状态
  keygen.go                           V1 密钥生成、RSA 2048 密钥对生成、merchant_no 生成

internal/domain/secret/               【新增】AES-256-GCM 封装
  secretbox.go                        Seal/Open（含 AAD）、主密钥版本与指纹自检

internal/domain/order/order.go        【改】加 MerchantID/Protocol/EpayType/MerchantOrderName
internal/port/ports.go                【改】MerchantRepository、MerchantKeyRepository；
                                          FindByPayID* 增加 merchantID 参数
internal/adapter/mysql/merchant_repository.go      【新增】
internal/adapter/mysql/merchant_key_repository.go  【新增】
internal/adapter/mysql/models.go      【改】OrderRow 加 4 列；ToDomain/orderToRow 双向映射
internal/adapter/mysql/order_repository.go         【改】按 pay_id 的 5 个方法加商户维度；
                                                          1062 分流（区分 public_token 与 pay_id 冲突）
internal/adapter/mysql/schema.sql     【改】新增 merchants/merchant_keys、改 orders 索引
database/schema.sql                   【改】同上（两份必须同步）
database/migrations/                  【新增】001_merchants.sql 等，配合 schema_migrations

internal/usecase/merchant_service.go  【新增】创建/轮换/启停、明文只读一次、审计
internal/usecase/epay_service.go      【新增】EPay 下单、幂等、查单
  └─ 复用 OrderService 的 acquirePrice / resolvePayURL（导出包装，不搬迁实现）
internal/usecase/order_service.go     【改】helper 导出；notify/return 禁止回落；
                                          ReturnURL 三分支；Create 行为不变
internal/usecase/monitor_service.go   【改】markPaid 按 protocol 生成载荷；签名失败不回滚支付
internal/usecase/notification_service.go 【改】topic 分派、载荷版本兼容、GET/POST 透传
internal/usecase/fakes_test.go        【改】stub 签名（端口变更的连锁编译）

internal/http/handler/epay_v1.go      【新增】submit.php / mapi.php / api.php
internal/http/handler/epay_v2.go      【新增】/api/pay/*
internal/http/handler/merchant.go     【新增】管理端 CRUD
internal/http/handler/handler.go      【改】Dependencies/Handlers 加字段（含 nil 兜底）
internal/http/router.go               【改】挂载根路径路由、限流、WriteGuardAlways
internal/config/config.go             【改】读 VMQ_MASTER_KEY；EPay 限流项（结构体/默认值/解析三处）
internal/app/app.go                   【改】装配新仓库与服务
cmd/vmqfox-api/main.go                【改】-migrate、-gen-master-key、-rotate-master-key

web/src/views/merchants/index.tsx     【新增】商户管理页（明文只揭示一次 + 轮换）
web/src/api/index.ts                  【改】商户接口类型与 client
web/src/views/api-doc/index.tsx       【改】三协议文档页
docker-compose.yml / env.example / env.production.example / docs/升级与迁移.md 【改】主密钥与升级步骤
```

**分层纪律**：`internal/domain/epay` 只做字符串与字节运算，不 import `database/sql`、
不 import `gin`、不碰 `context`。这样签名算法可以用官方报文做纯函数测试，
也避免协议细节渗进业务层。

**不要做的事**：不要把 `OrderService.Create` 拆开重写。`order_payurl_test.go` 直接以
`&OrderService{qrcodes: …}` 构造对象调用私有方法，搬迁实现会连带打红测试；
只给 `acquirePrice` / `resolvePayURL` 加导出包装即可。

---

## 8. 实施阶段

| 阶段 | 内容 | 验收（可执行判据） |
| :-- | :-- | :-- |
| **P0** | `merchants`/`merchant_keys` 建表、AES-GCM 封装（含 AAD）、`schema_migrations` + `-migrate`、主密钥读取（先只读不强制） | 迁移脚本在空 5.7 库连跑两次幂等；`secretbox_test` 覆盖 Seal/Open/篡改 nonce/主密钥轮换双读；未配主密钥时服务仍能启动 |
| **P1** | V1 签名/验签 + `submit.php` + `mapi.php` + 回调（GET）+ 幂等 | 附录 A 的 `sign` 逐字符等于给定值；用独角数卡**默认配置（页面跳转）**完成一笔 0.01 元支付，商户侧订单变为已支付且回调 1 次成功；同 `out_trade_no` 连打两次返回同一 `payurl` |
| **P2** | `api.php`（仅 `act=order`）+ 管理端商户 CRUD/轮换 | `act=order` 返回字段齐全，已支付订单 `trade_status=TRADE_SUCCESS`；轮换后旧密钥仍可验签，明文只返回一次 |
| **P3** | V2 签名 + `/api/pay/create`、`query`、`close`（彩虹/ezfp 系） | **前置门禁**：附录 C 的三项联调核对完成 + 一笔真实报文进仓库。`timestamp` 为 now-301s 返回 `code=-1`、now-299s 返回成功；投递时重签后，停机 30 分钟的商户恢复仍能收通知 |
| **P4** | 多商户隔离审计 + 运营能力 | 跨商户查单/关单返回与「不存在」逐字节相同；订单导出 CSV 含 `price`/`reallyPrice`/`merchant_no`；管理端展示金额冲突率与实收口径 |
| **P5** | 存量升级演练 | 旧库备份 → `-migrate` → 新版本启动 → 原生 `/api/order/create` 回归通过；索引切换后 `SHOW INDEX` 与查询计划符合预期 |

**排序原则**：协议层纯函数先做完（含黄金用例）再动 schema——这一点保留原设计；
但 `orders.merchant_id` 必须在 P1 之前落库，因为回调要按商户签名，
而 `markPaid` 只拿得到 `order.Order`，没有 `merchant_id` 就选不出密钥。
`epay_type` / `merchant_order_name` 可以延后（`payment_type` 与 `type` 是一对一，可反查）。

---

## 9. 风险与取舍

| 风险 | 影响 | 应对 |
| :-- | :-- | :-- |
| **存量升级无迁移通道** | 新版本连旧库直接 `unknown column`，建单 500 | `schema_migrations` + `-migrate`；两份 `schema.sql` 同步；升级文档前置「先迁移再启动」 |
| **主密钥未配/配错** | 未配：早期拒绝启动会让存量部署重启即宕机；配错：解密全失败但界面显示正常 | 条件必填 + 启动指纹自检；`-gen-master-key`；`VMQ_MASTER_KEY_OLD` 双读 |
| 多商户共享金额空间 | 同额并发下单会失败；热门价位可被单商户占满 | 商户级待支付并发/建单配额；金额冲突率指标；产品说明写明物理限制 |
| 微调导致商户对账差 | 实收比订单多 0~0.09 元 | 候选必须存在精确码；导出并列 `price`/`reallyPrice`；统计拆双口径 |
| 精确码覆盖不足 | 微调后无码可扫 → 掉单 | 下单前校验候选码存在性，不满足则不微调 |
| 回调地址不合规 | 付款成功但通知永远失败（约 8 小时后死信） | 下单期预校验 scheme/IP；出厂 CIDR 白名单改为空 |
| V2 分支差异 | 目标发卡站在 `timestamp` 格式 / `sign_type` 签名域 / `channel` 别名三点上与锚定结论不一致 | 已锚定彩虹/ezfp 系；附录 C 列三项核对，以真实报文为准 |
| V2 通知时效 | 固化签名在 +7 分钟后必然过期 | V2 入队存参数、投递时重签 |
| 支付事务内做密码学 | 一个解密失败放大为「全部到账无法核销」 | 核销与通知解耦；签名失败落通知失败态 |
| 密钥轮换窗口期漏签 | 旧订单回调用新密钥 | V1 签名入队固化；V2 固定用 `generation=1` 重签 |
| 主密钥丢失 | 所有商户密钥不可解密 | 文档强调备份；`-rotate-master-key` 子命令；恢复路径写明「重置全部商户密钥」 |
| 多商户弱化单商户假设 | 现有 `singleton_guard` 逻辑与新表并存 | 管理员仍唯一（管后台），商户是业务维度；`REFACTOR-PLAN` 需同步标注该非目标被部分推翻 |

---

## 10. 确认状态

### 已确认（2026-11-13 初版，2026-11-14 增补）

- **角色**：本网关扮演 EPay 平台侧，站点填「网关 / PID / 密钥」三格即可接入。
- **商户模型**：多商户但**站内自控、不开放注册**；内部 `id` 主键，对外 `merchant_no` 作 pid。
- **商户关系**：网关不经手资金、不做结算与分账（§0.3）；但必须做商户级配额与审计。
- **协议版本**：V1（MD5）优先并独立上线；V2 按彩虹/ezfp 系实现，排在 P3（附录 C）。
- **金额策略**：默认开启微调（`epayPriceAdjust=1`），候选必须存在精确金额码
  （`epayRequireExactCode=1`），只增不减；用尽候选返回失败。
- **结算口径**：以 `really_price_cent`（实收）为准，分差归商户。
- **密钥隔离**：每商户每协议独立密钥；V2 平台密钥对按商户生成；`peer_public_key` 不随轮换改变。
- **回调投递**：复用 Outbox 存储/租约/退避/死信；投递层扩展；V1 固化、V2 重签。
- **支付与通知**：核销必成功、通知可失败，二者不共享事务。
- **V2 锚定（2026-11-14）**：按**彩虹/ezfp 系**实现——`pid` + `money` 元 + `code=1` +
  `timestamp` + `/api/pay/*`，字段与 V1 同构（附录 C）；不追求兼容全部分支。
- **订单有效期（2026-11-14）**：EPay 订单按 `merchants.close_minutes`，**默认 30 分钟**（0 则回落全局）；
  原生订单仍只看全局 `settings.close`，行为不变。
- **商户级配额（2026-11-14）**：待支付并发 **50** / 建单 **10 次每分钟**（占位值，P4 按试点数据校准）。

### 待定项

暂无。V2 锚定、订单有效期、配额默认值三项已于 2026-11-14 拍板；
配额数值属可调参数，不阻塞开工。

---

## 附录 A：V1 签名黄金用例

```go
// 输入（注意 money 保留 "12.50" 而非 "12.5"）
params := map[string]string{
    "pid":          "M7K3QPX9Z2AB4C",
    "type":         "alipay",
    "out_trade_no": "M20261113001",
    "notify_url":   "https://m.example.com/notify",
    "return_url":   "https://m.example.com/return",
    "name":         "测试商品",
    "money":        "12.50",
}
key := "abcdefghjkmnpqrstuvwxyz23456789k"   // 32 位，字符集见 §4.2

// 待签串（ASCII 升序，剔除空值，末尾直接接 key），共 166 字节
// money=12.50&name=测试商品&notify_url=https://m.example.com/notify
// &out_trade_no=M20261113001&pid=M7K3QPX9Z2AB4C&return_url=https://m.example.com/return
// &type=alipayabcdefghjkmnpqrstuvwxyz23456789k

// sign = md5(上述字符串)，小写十六进制
// 黄金值：51978ac227dba94b887893ca5ba14917
```

注意 `type=alipay` 后面**紧接**密钥，没有 `&`。这是 V1 实现里最常见的错误点。

> **口径说明**：`merchant_no` 固定 14 位（`M` + 13 位），密钥固定 32 位；两者改一位，上面的黄金值就必须重算。
> 黄金值用 `printf '%s' '<待签串><key>' | md5sum` 可独立复现。

**零值用例**（证明 `"0"` 不是空串，不能被剔除）：

```
待签串（173 字节）：money=0.00&name=测试商品&notify_url=https://m.example.com/notify
        &out_trade_no=M20261113001&param=0&pid=M7K3QPX9Z2AB4C
        &return_url=https://m.example.com/return&type=alipay
        + key
sign = da925ce317b29493d22c7342ec358291
```

若实现把 `param=0` 或 `money=0.00` 当空值剔除，待签串会少一段，签名必然对不上。

**回调签发用例**（同 key，订单已支付）：

```
待签串（137 字节）：money=12.50&name=测试商品&out_trade_no=M20261113001&pid=M7K3QPX9Z2AB4C
        &trade_no=20261113000001&trade_status=TRADE_SUCCESS&type=alipay
        + key
sign = 9bea14e3e7b6f68ac4e479b9c3c9b3c5

发送串：money=12.50&name=%E6%B5%8B%E8%AF%95%E5%95%86%E5%93%81
        &out_trade_no=M20261113001&pid=M7K3QPX9Z2AB4C&trade_no=20261113000001
        &trade_status=TRADE_SUCCESS&type=alipay&sign=9bea14e3e7b6f68ac4e479b9c3c9b3c5&sign_type=MD5
```

**已知取舍**：为消除待签串二义性，参数值里含 `&` 或 C0 控制字符（`\r`/`\n` 等）一律拒绝（`ErrAmbiguousParam`）。
这意味着 `return_url=https://x/r?a=1&b=2` 这类带多参数的地址会被拒——对外统一回「签名错误」，
内部审计单独记「参数含歧义分隔符或控制字符」，否则线上会被误诊成密钥配错；
用户手册必须写清替代方案（业务参数塞进**单个** `param` 值，或用路径段承载），否则会变成反复工单。

签署用原始值、发送用编码值。两条都要进单测：同一份参数分别断言 sign 与最终 URL。

## 附录 B：与现有能力的复用关系

| 现有能力 | EPay 是否复用 | 说明 |
| :-- | :-- | :-- |
| `price_locks` 金额占锁 | ✅ 完全复用 | 全局唯一，不加商户维度 |
| `qrcodes` 选码 | ✅ 复用，但**微调候选必须命中精确码** | 含通用码 `price_cent=0` 回落，EPay 微调路径禁用回落 |
| `outbox` 存储/租约/退避/死信 | ✅ 复用 | 载荷与 topic 需扩展 |
| `notificationSucceeded` | ✅ 完全复用 | `success`/空正文判定已兼容 |
| `NotificationService.deliver` | ❌ **需改** | topic 白名单、方法、载荷校验三处 |
| `public_token` 收银台 | ✅ 完全复用 | `payurl` 指向同一页面 |
| 支付宝转账确认单 | ✅ 自动继承 | 收银台已支持，EPay 订单同样生效 |
| `settings.key` 原生验签 | ✅ 保留 | 与 EPay 商户密钥互不影响 |
| `OrderService.Create` | ⚠️ 只复用两个 helper | 验签、密钥依赖、通知地址回落均不适用 |
| 挂机端协议 | ❌ 零改动 | 与协议无关 |
| 同步回跳 `ReturnURL()` | ❌ **需改** | 按 protocol 三分支，否则 EPay 回跳验签失败 |

## 附录 C：V2 锚定结论（彩虹/ezfp 系）

**已定（2026-11-14）**：V2 按彩虹/ezfp 系实现，不追求兼容全部分支。

| 维度 | 取值 |
| :-- | :-- |
| 商户号 | `pid`（本网关填 `merchant_no`） |
| 金额 | `money`，单位**元**，最多两位小数的字符串 |
| 成功码 | `code=1` |
| 签名 | SHA256WithRSA（PKCS#1 v1.5 + SHA-256）；剔除 `sign`/`sign_type`/空值，参数名 ASCII 升序，值不做 URL 编码，结果 Base64 |
| 时间戳 | 请求、响应、回调都带 `timestamp` 并参与签名，商户侧 ±300s 校验 |
| 接口地址 | `/api/pay/create`、`/api/pay/query`、`/api/pay/close` |

### 联调首日必须核对的三项（不阻塞设计，阻塞上线）

1. `timestamp` 是 10 位秒级还是 ISO8601；
2. `sign_type` 是否进签名域（本网关按"剔除"实现，若对方含入则加配置项切换）；
3. 回调的支付方式字段是 `type` 还是 `channel`（见 §6.2 的双发约定）。

### 黄金用例（P3 门禁）

拿一笔真实的「下单请求 + 下单响应 + 回调」报文进仓库做 golden test。
若目标发卡站插件与上述三项不一致，**以真实报文为准**并回写本附录。

## 附录 D：变更记录（v3.3 → v3.4）

| # | 变更 | 依据 |
| :-- | :-- | :-- |
| 1 | 撤回「投递层零改动」，改「复用存储/租约/退避 + 投递层扩展」 | `notification_service.go:91-111` 硬编码；换载荷会静默丢包 |
| 2 | 签名策略拆为 V1 固化 / V2 重签 | V2 `timestamp` ±300s 与 20 次退避互斥 |
| 3 | 新增 §6.6 支付核销与通知解耦 | `markPaid` 在支付事务内做解密/签名，失败会回滚核销与去重 |
| 4 | 金额微调增加「精确码存在性」前提；补结算与对账口径 | `resolvePayURL` 回落通用码会掉单；`Statistics` 用 `price_cent` |
| 5 | 新增 §2.4 迁移机制、`schema_migrations`、两份 schema 同步、单条 ALTER 与回滚边界 | 仓库无迁移框架；5.7 无 INSTANT；拆句留窗口 |
| 6 | 主密钥改条件必填 + 指纹自检 + AAD + `key_version` | 存量升级即宕机；GCM 缺 AAD 可跨行错用 |
| 7 | 新增 §6.4 同步回跳三分支 | `ReturnURL()` 仅支持 VMQ v2 字段 |
| 8 | 新增 §5.4 幂等规则 | 重试会产生「商户认为失败、用户已付款」 |
| 9 | 新增 §6.5 `notify_url` 预校验；修正出厂 CIDR 白名单 | 下单放行 http、投递端拒绝 |
| 10 | `pid` 改为 `merchant_no`；补商户级配额 | 自增 ID 可枚举；金额空间可被打满 |
| 11 | `act` 收窄为仅 `order`；补 `act=order` 响应字段表；`param` 落库回显 | 无余额/结算实体；原版会回显密钥 |
| 12 | V2 锚定彩虹/ezfp 系（字段与 V1 同构 + `timestamp` + `/api/pay/*`），附录 C 改为「锚定结论 + 三项联调核对」 | 生态内 V2 字段/单位/成功码互不兼容，必须选一支 |
| 13 | §7 落位补齐 12 处，`server.go` 更正为 `router.go` | `internal/http/server.go` 不存在 |
| 14 | §8 验收改为可执行判据；P1 纳入 `submit.php` | 原验收「跑通」不可证伪；目标客户默认走页面跳转 |
| 15 | 商户增加 `close_minutes`（默认 30，0=跟随全局）；EPay 订单按商户取有效期 | 发卡站支付窗口普遍 30 分钟，本地 5 分钟会产生「已关闭才到账」 |
| 16 | 固化商户级配额默认值（待支付 50 / 建单 10 次每分） | 金额空间全局共享，必须有配额兜底 |
