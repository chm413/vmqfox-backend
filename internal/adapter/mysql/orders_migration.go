package mysql

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// orders 表上的商户维度改造（设计文档 §2.3 / §2.4 第 2、4 步）。
//
// 为什么是 Go 迁移而不是 SQL 脚本：MySQL 5.7 不支持 ADD COLUMN IF NOT EXISTS，
// 也没有 CREATE INDEX IF NOT EXISTS。002/003 的 DDL 只能用「先探测再改」的方式幂等，
// 而这种判断写不进纯 SQL。001_merchants 能留在 migrations/ 下，是因为 CREATE TABLE IF NOT EXISTS 够用。
const (
	ordersTable = "orders"

	// legacyPayIDIndex 是改造前的全局唯一索引，改造后必须消失：
	// 只要它还在，两个商户用同一个 out_trade_no 就会被拒，多商户等于没做。
	legacyPayIDIndex = "uq_orders_pay_id"
	// merchantPayIDIndex 是改造后的商户内唯一索引，merchant_id 必须在最左，
	// 否则按商户查单会失去最左前缀，走全表扫描。
	merchantPayIDIndex = "uq_orders_merchant_pay_id"
	// merchantStateIndex 服务「某商户的待支付/已关闭订单」这类运营查询。
	merchantStateIndex = "idx_orders_merchant_state"
)

// ordersMerchantColumn 描述一列：名字、定义、相对位置。
// 顺序与两份 schema.sql 的 orders 段保持一致，让「升级库」与「新装库」的列顺序相同，
// 否则 mysqldump 对比、SELECT * 的列序都会出现无意义差异。
type ordersMerchantColumn struct {
	name       string
	definition string
	after      string
}

var ordersMerchantColumns = []ordersMerchantColumn{
	{name: "merchant_id", definition: "BIGINT NOT NULL DEFAULT 0", after: "id"},
	{name: "protocol", definition: "TINYINT NOT NULL DEFAULT 0", after: "merchant_id"},
	{name: "epay_type", definition: "VARCHAR(16) NOT NULL DEFAULT ''", after: "protocol"},
	{name: "merchant_order_name", definition: "VARCHAR(128) NOT NULL DEFAULT ''", after: "epay_type"},
}

// orderMigrations 注册 orders 相关的 Go 迁移，版本号与 migrations/ 下的 SQL 脚本共用同一序列。
func orderMigrations() []GoMigration {
	return []GoMigration{
		{Version: 2, Name: "orders_merchant_columns", Up: upOrdersMerchantColumns},
		{Version: 3, Name: "orders_merchant_index", Up: upOrdersMerchantIndex},
	}
}

// upOrdersMerchantColumns 一次性加上 4 列。
//
// 必须是一条 ALTER：MySQL 5.7 没有 ALGORITHM=INSTANT，拆成 4 条会把这棵表重建 4 次，
// 订单表越大停机越久。4 列都是「NOT NULL + 常量默认值」，存量行由 MySQL 直接填默认值，
// merchant_id=0 正好是「系统商户」，存量订单与原生接口语义不变。
func upOrdersMerchantColumns(db *gorm.DB) error {
	if err := requireOrdersTable(db); err != nil {
		return err
	}

	missing := make([]ordersMerchantColumn, 0, len(ordersMerchantColumns))
	for _, column := range ordersMerchantColumns {
		if !db.Migrator().HasColumn(ordersTable, column.name) {
			missing = append(missing, column)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	if err := db.Exec(addColumnsStatement(missing)).Error; err != nil {
		return fmt.Errorf("为 orders 添加商户维度列失败: %w", err)
	}
	return nil
}

// addColumnsStatement 拼出「一条 ALTER 加 N 列」的语句。
// 单独抽出来是为了能在没有 MySQL 的环境里钉住语句形态：
// 拆成 N 条会重建 N 次表，这是 §2.4 明确要避免的事故。
//
// 依赖：AFTER 引用的可能是本语句中先加的列（protocol AFTER merchant_id）。
// 这条路径没有在本机 MySQL 上跑过（仓库 CI 也没有 5.7 服务），若某个版本报
// ERROR 1054 Unknown column 'merchant_id' in 'orders'，把 AFTER 子句整段去掉即可：
// 列会落到表尾，功能完全不受影响，只影响列顺序与 schema.sql 的对齐。
func addColumnsStatement(columns []ordersMerchantColumn) string {
	clauses := make([]string, 0, len(columns))
	for _, column := range columns {
		clauses = append(clauses, fmt.Sprintf("ADD COLUMN %s %s AFTER %s", column.name, column.definition, column.after))
	}
	return "ALTER TABLE " + ordersTable + " " + strings.Join(clauses, ", ")
}

// upOrdersMerchantIndex 把 pay_id 的唯一性从「全局」收敛为「商户内」。
//
// 单条 ALTER 同时 DROP + ADD 是关键：分开执行会出现「旧索引已删、新索引未建」的间隙，
// 间隙内并发下单可写入重复 pay_id，之后建唯一索引必然 1062。
// 合进一条语句后，ALTER 全程持有 MDL，其它会话只会看到「改前」或「改后」的表定义，
// 读写都落在一个始终有 (merchant_id, pay_id) 唯一约束的表上。
// （诚实边界：MySQL 5.7 的 DDL 不是崩溃原子的，这里解决的是并发可见性，不是断电恢复。）
func upOrdersMerchantIndex(db *gorm.DB) error {
	if err := requireOrdersTable(db); err != nil {
		return err
	}
	// 002 必须先跑：索引切换依赖 merchant_id 列存在。
	// 版本号机制保证了顺序，这里再挡一次，避免有人手工改了 schema_migrations。
	for _, column := range []string{"merchant_id", "pay_id"} {
		if !db.Migrator().HasColumn(ordersTable, column) {
			return fmt.Errorf("orders.%s 不存在：请先应用迁移 002_orders_merchant_columns", column)
		}
	}

	legacyExists := db.Migrator().HasIndex(ordersTable, legacyPayIDIndex)
	merchantExists := db.Migrator().HasIndex(ordersTable, merchantPayIDIndex)
	stateExists := db.Migrator().HasIndex(ordersTable, merchantStateIndex)

	switch {
	case merchantExists && !legacyExists:
		// 正常终态。只剩二级索引缺失（上次执行到一半、DDL 已提交但版本未写入）时单独补齐。
		if stateExists {
			return nil
		}
		if err := db.Exec("ALTER TABLE " + ordersTable + " ADD KEY " + merchantStateIndex + " (merchant_id, state, created_at)").Error; err != nil {
			return fmt.Errorf("为 orders 补充 %s 索引失败: %w", merchantStateIndex, err)
		}
		return nil

	case merchantExists && legacyExists:
		// 有人手工提前建过新索引：只需摘掉全局唯一索引。
		// 此时留两套唯一约束会让「跨商户同单号」在运行时被拒，症状极难排查。
		if err := db.Exec("ALTER TABLE " + ordersTable + " DROP INDEX " + legacyPayIDIndex).Error; err != nil {
			return fmt.Errorf("删除遗留全局唯一索引 %s 失败: %w", legacyPayIDIndex, err)
		}
		return nil

	case !merchantExists && !legacyExists:
		// 两个索引都没有：pay_id 完全不受唯一约束保护，不是迁移器该自动猜的状态。
		return errors.New("orders 上既没有 uq_orders_pay_id 也没有 uq_orders_merchant_pay_id，请人工确认索引状态后再迁移")
	}

	statement := switchIndexStatement(!stateExists)
	if err := db.Exec(statement).Error; err != nil {
		return fmt.Errorf("切换 orders 唯一索引为 (merchant_id, pay_id) 失败: %w", err)
	}
	return nil
}

// switchIndexStatement 拼出「DROP 旧唯一索引 + ADD 新唯一索引（+ 二级索引）」的单条 ALTER。
// 抽成纯函数同样是为了在无 MySQL 的环境里钉住「必须是一条语句」这个不变量。
func switchIndexStatement(addStateIndex bool) string {
	statement := "ALTER TABLE " + ordersTable +
		" DROP INDEX " + legacyPayIDIndex +
		", ADD UNIQUE KEY " + merchantPayIDIndex + " (merchant_id, pay_id)"
	if addStateIndex {
		statement += ", ADD KEY " + merchantStateIndex + " (merchant_id, state, created_at)"
	}
	return statement
}

// requireOrdersTable 让「先 -migrate 再 -init-db」这种顺序错误立刻显形。
// 静默跳过最危险：版本表会记下「已应用」，之后 -init-db 建出来的表就永远缺这些列。
func requireOrdersTable(db *gorm.DB) error {
	if !db.Migrator().HasTable(ordersTable) {
		return errors.New("orders 表不存在：请先用 -init-db 初始化基础表结构，再执行 -migrate")
	}
	return nil
}
