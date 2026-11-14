-- Go-only 初始 Schema（MySQL 5.7+）。
-- 这是全新部署脚本，不执行旧 PHP 四表迁移，也不包含运行时建表逻辑。
-- 所有业务状态转换依赖 InnoDB 事务与行锁。

CREATE TABLE settings (
  setting_key VARCHAR(64) NOT NULL,
  setting_value TEXT NOT NULL,
  PRIMARY KEY (setting_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE admin_credentials (
  id TINYINT UNSIGNED NOT NULL,
  username VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  password_hash VARBINARY(255) NOT NULL,
  enabled TINYINT(1) NOT NULL DEFAULT 1,
  singleton_guard TINYINT UNSIGNED GENERATED ALWAYS AS (1) STORED,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_admin_credentials_singleton (singleton_guard)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE qrcodes (
  id BIGINT NOT NULL AUTO_INCREMENT,
  pay_url TEXT NOT NULL,
  price_cent BIGINT NOT NULL,
  payment_type TINYINT NOT NULL,
  state TINYINT NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_qrcodes_lookup (payment_type, state, price_cent, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE orders (
  id BIGINT NOT NULL AUTO_INCREMENT,
  order_id VARBINARY(128) NOT NULL,
  public_token CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  pay_id VARBINARY(255) NOT NULL,
  payment_type TINYINT NOT NULL,
  price_cent BIGINT NOT NULL,
  really_price_cent BIGINT NOT NULL,
  price_text VARCHAR(64) NOT NULL,
  really_price_text VARCHAR(64) NOT NULL,
  state TINYINT NOT NULL DEFAULT 0,
  param VARCHAR(255) NOT NULL DEFAULT '',
  pay_url TEXT NOT NULL,
  is_auto TINYINT(1) NOT NULL DEFAULT 0,
  notify_url TEXT NOT NULL,
  return_url TEXT NOT NULL,
  created_at DATETIME(6) NOT NULL,
  paid_at DATETIME(6) NULL,
  closed_at DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_orders_order_id (order_id),
  UNIQUE KEY uq_orders_public_token (public_token),
  UNIQUE KEY uq_orders_pay_id (pay_id),
  KEY idx_orders_payment_state_amount (payment_type, state, really_price_cent, id),
  KEY idx_orders_state_created (state, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE price_locks (
  payment_type TINYINT NOT NULL,
  amount_cent BIGINT NOT NULL,
  order_id VARBINARY(128) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (payment_type, amount_cent),
  UNIQUE KEY uq_price_locks_order_id (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE payment_events (
  event_key CHAR(64) CHARACTER SET ascii NOT NULL,
  payment_type TINYINT NOT NULL,
  price_cent BIGINT NOT NULL,
  event_time VARCHAR(64) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (event_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE notification_outbox (
  id CHAR(64) CHARACTER SET ascii NOT NULL,
  topic VARCHAR(64) CHARACTER SET ascii NOT NULL,
  aggregate_id VARBINARY(128) NOT NULL,
  event_key CHAR(64) CHARACTER SET ascii NOT NULL,
  payload LONGTEXT NOT NULL,
  status TINYINT NOT NULL DEFAULT 0,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL,
  lease_token CHAR(64) CHARACTER SET ascii NOT NULL DEFAULT '',
  locked_until DATETIME(6) NULL,
  last_error TEXT NULL,
  created_at DATETIME(6) NOT NULL,
  delivered_at DATETIME(6) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_outbox_event_topic (event_key, topic),
  KEY idx_outbox_claim (status, next_attempt_at, locked_until, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 商户主体：内部 id 不对外，对外只暴露 merchant_no；订单有效期按商户独立配置。
-- merchant_no 是对外 pid，用 ascii_bin 精确匹配：库默认 utf8mb4_unicode_ci 下
-- 'M7K...' 与 'm7k...' 会命中同一行，对外标识不允许这种模糊。
CREATE TABLE merchants (
  id            BIGINT NOT NULL AUTO_INCREMENT,
  merchant_no   CHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  name          VARCHAR(64) NOT NULL DEFAULT '',
  enabled       TINYINT(1) NOT NULL DEFAULT 1,
  close_minutes INT NOT NULL DEFAULT 30,
  note          VARCHAR(255) NOT NULL DEFAULT '',
  created_at    DATETIME(6) NOT NULL,
  updated_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uq_merchants_no (merchant_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 商户密钥：两代制，每商户每协议最多两行（generation=1 当前，0 仅验签）。
-- 协议是否开通由「是否存在 generation=1 的行」决定，不额外设开关。
CREATE TABLE merchant_keys (
  merchant_id     BIGINT NOT NULL,
  protocol        TINYINT NOT NULL,
  generation      TINYINT NOT NULL,
  key_version     SMALLINT NOT NULL DEFAULT 1,
  secret_cipher   BLOB NOT NULL,
  secret_nonce    BLOB NOT NULL,
  -- 不能用 TEXT：MySQL 5.7 禁止 TEXT/BLOB 带 DEFAULT（ERROR 1101），而本仓库固定 mysql:5.7。
  -- PKIX PEM 长度：RSA-2048 ≈451 字节、RSA-4096 ≈800 字节，VARCHAR(1024) 足够且可保留默认值。
  peer_public_key VARCHAR(1024) NOT NULL DEFAULT '',
  hint            VARCHAR(16) NOT NULL DEFAULT '',
  last_used_at    DATETIME(6) NULL,
  created_at      DATETIME(6) NOT NULL,
  PRIMARY KEY (merchant_id, protocol, generation)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 管理员凭据不提供默认值。部署者必须在启用服务前写入 id=1 的账号和 bcrypt 哈希；
-- 应用启动时不会创建、修改或补全该记录。
-- 示例结构：INSERT INTO admin_credentials
--   (id, username, password_hash, enabled, created_at, updated_at)
--   VALUES (1, '<管理员账号>', '<bcrypt 哈希>', 1, CURRENT_TIMESTAMP(6), CURRENT_TIMESTAMP(6));

-- 初始设置只提供非凭据默认值；生产通讯密钥与收款配置通过管理设置流程写入。
INSERT INTO settings (setting_key, setting_value) VALUES
  ('key', ''),
  ('notifyUrl', ''),
  ('returnUrl', ''),
  ('lastheart', '0'),
  ('lastpay', '0'),
  ('jkstate', '-1'),
  ('close', '5'),
  ('payQf', '1'),
  ('wxpay', ''),
  ('zfbpay', '');