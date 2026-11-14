package mysql

import (
	"fmt"
	"strings"
	"time"

	"github.com/hulisang/vmqfox-backend/database"
	"gorm.io/gorm"
)

// schemaMigrationsTable 记录已应用的迁移版本。
// 注意：设计文档 §2.4 里写的是 schema_version，实际落地用 schema_migrations，
// 因为它同时保存 name（便于人工核对是哪个脚本），语义也更贴近「已应用清单」。
const schemaMigrationsTable = "schema_migrations"

// schemaMigrationsDDL 建版本表本身不纳入 migrations/ 目录，否则无法记录「第一次迁移已应用」。
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS ` + schemaMigrationsTable + ` (
  version    INT NOT NULL,
  name       VARCHAR(128) NOT NULL,
  applied_at DATETIME(6) NOT NULL,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// SchemaMigrationRow 是版本表映射。
type SchemaMigrationRow struct {
	Version   int       `gorm:"column:version;primaryKey"`
	Name      string    `gorm:"column:name;not null"`
	AppliedAt time.Time `gorm:"column:applied_at;not null"`
}

func (SchemaMigrationRow) TableName() string { return schemaMigrationsTable }

// Migrate 按版本号升序执行尚未应用的迁移，返回本次新应用的条数。
//
// 幂等：连续执行两次，第二次返回 0 且不产生任何写入。
// 事务边界（诚实表述）：MySQL 的 DDL 会隐式提交，InnoDB 无法回滚建表/改表，
// 因此这里的事务只能保证「迁移记录与实际执行结果同生共死」，
// 语句层面的失败仍需靠脚本自身幂等（CREATE TABLE IF NOT EXISTS）重跑才能收敛。
func Migrate(db *gorm.DB) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("数据库连接未初始化")
	}
	if err := db.Exec(schemaMigrationsDDL).Error; err != nil {
		return 0, fmt.Errorf("创建迁移版本表 %s 失败: %w", schemaMigrationsTable, err)
	}

	migrations, err := database.Migrations()
	if err != nil {
		return 0, fmt.Errorf("加载迁移脚本失败: %w", err)
	}

	appliedVersions, err := appliedMigrationVersions(db)
	if err != nil {
		return 0, err
	}

	applied := 0
	for _, migration := range migrations {
		if _, done := appliedVersions[migration.Version]; done {
			continue
		}
		executed, err := applyMigration(db, migration)
		if err != nil {
			return applied, err
		}
		if !executed {
			// 并发执行时被其它进程抢先应用，本进程不重复执行。
			continue
		}
		applied++
	}
	return applied, nil
}

// appliedMigrationVersions 读取已应用的版本集合。
func appliedMigrationVersions(db *gorm.DB) (map[int]struct{}, error) {
	var versions []int
	if err := db.Model(&SchemaMigrationRow{}).Pluck("version", &versions).Error; err != nil {
		return nil, fmt.Errorf("读取已应用迁移版本失败: %w", err)
	}
	result := make(map[int]struct{}, len(versions))
	for _, version := range versions {
		result[version] = struct{}{}
	}
	return result, nil
}

// applyMigration 在单个事务内执行一条迁移脚本并写入版本记录。
// 返回 false 表示该版本已被其它进程应用（事务内二次确认，避免重复建表报错）。
func applyMigration(db *gorm.DB, migration database.Migration) (bool, error) {
	executed := false
	err := db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&SchemaMigrationRow{}).Where("version = ?", migration.Version).Count(&count).Error; err != nil {
			return fmt.Errorf("查询迁移版本 %d 失败: %w", migration.Version, err)
		}
		if count > 0 {
			return nil
		}

		statements := splitMigrationStatements(migration.SQL)
		if len(statements) == 0 {
			return fmt.Errorf("迁移 %03d_%s 没有任何可执行语句", migration.Version, migration.Name)
		}
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("迁移 %03d_%s 执行失败 [%s]: %w", migration.Version, migration.Name, statement, err)
			}
		}

		record := SchemaMigrationRow{
			Version:   migration.Version,
			Name:      migration.Name,
			AppliedAt: time.Now().UTC(),
		}
		if err := tx.Create(&record).Error; err != nil {
			return fmt.Errorf("写入迁移版本 %d 失败: %w", migration.Version, err)
		}
		executed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return executed, nil
}

// splitMigrationStatements 按 `;` 切分多条语句并丢弃 `--` 注释行。
// 与 schema.go 的 splitSQLStatements 语义保持一致，但各自独立实现，
// 避免迁移逻辑与整库初始化逻辑互相牵制（改一处不影响另一处）。
func splitMigrationStatements(sqlContent string) []string {
	var statements []string
	var current strings.Builder
	for _, line := range strings.Split(sqlContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
				statements = append(statements, trimmed)
			}
			current.Reset()
		}
	}
	if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
		statements = append(statements, trimmed)
	}
	return statements
}
