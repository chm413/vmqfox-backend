-- 001_merchants：EPay 多商户基础表（无侵入，不改动任何既有表）。
-- 与 internal/adapter/mysql/schema.sql 中的同名表定义保持一致；
-- 迁移必须幂等，因此一律使用 CREATE TABLE IF NOT EXISTS（MySQL 的 DDL 不支持事务回滚，
-- 中途失败重跑时靠 IF NOT EXISTS 续跑到成功）。
--
-- merchant_no 是对外 pid，列上强制 ascii_bin 精确匹配：库默认 utf8mb4_unicode_ci 下
-- 'M7K...' 与 'm7k...' 会命中同一行，对外标识不允许这种模糊。

CREATE TABLE IF NOT EXISTS merchants (
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

CREATE TABLE IF NOT EXISTS merchant_keys (
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
