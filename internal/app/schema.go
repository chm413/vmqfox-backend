package app

// 启动前的 schema 自检。
//
// 为什么需要它：服务启动本身不做任何建表/迁移，只有 `-init-db`/`-init-admin`/`-migrate`
// 子命令会动结构。于是「升级了新二进制但忘了跑迁移」在旧版本上的表现是——
// 进程正常起来、/health 200、/ready 也 200（它只 Ping 数据库），
// 只有生命周期任务每 10 秒刷一次 `Table 'xxx' doesn't exist`。
// 运维看到的是「服务没挂但一直报错」，而编排系统完全无感。
//
// 这里不做自动迁移：启动期隐式 DDL 在多副本滚动更新时会互相打架，
// 且迁移失败会让服务起不来却缺少可读的原因。行业惯例也是显式 migrate 或用 init container。
// 我们要做的是把「忘了迁移」从运行时噪音变成启动期的一声明确拒绝。

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// requiredSchemaTables 是服务能正常工作的最小表集合。
//
// 只列「缺了必然导致运行时错误」的表：settings 与 orders 是核心，merchants 与
// merchant_keys 是 EPay 多商户引入的（生命周期任务每轮都会读商户关单窗口）。
// 不列 payment_events/notification_outbox 这类可选链路表——缺它们只影响通知，
// 不该拦住整个服务启动。
var requiredSchemaTables = []string{"settings", "orders", "merchants", "merchant_keys"}

// EnsureSchema 在启动前确认关键表存在，缺失时返回带修复指令的错误。
func EnsureSchema(db *gorm.DB) error {
	if db == nil {
		return errors.New("数据库连接未初始化")
	}
	missing := missingSchemaTables(func(table string) bool {
		return db.Migrator().HasTable(table)
	})
	if len(missing) > 0 {
		return fmt.Errorf(
			"数据库缺少关键表 [%s]：请先执行 `vmqfox-api -migrate` 完成迁移（全新库请用 `-init-db` 或 `-init-admin`）",
			strings.Join(missing, ", "),
		)
	}
	return nil
}

// missingSchemaTables 抽成纯函数便于单测：只依赖一个「表是否存在」的判定，
// 不需要真库也能覆盖「缺哪张表、怎么排序」这些逻辑。
func missingSchemaTables(exists func(string) bool) []string {
	missing := make([]string, 0, len(requiredSchemaTables))
	for _, table := range requiredSchemaTables {
		if !exists(table) {
			missing = append(missing, table)
		}
	}
	return missing
}
