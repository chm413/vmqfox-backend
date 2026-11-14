package mysql

import (
	"fmt"
	"sort"
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

// GoMigration 是「必须先探测 schema 再改」的迁移。
//
// 为什么需要它：MySQL 5.7 没有 ADD COLUMN IF NOT EXISTS / CREATE INDEX IF NOT EXISTS，
// 而 §2.4 要求迁移可重复执行。ALTER TABLE 没有 CREATE TABLE IF NOT EXISTS 那样的兜底，
// 只能把「改没改过」的判断写进 Go（information_schema 探测列与索引）。
//
// 契约：Up 必须自己判断「已经改过」并直接返回 nil。runner 只保证同一 version 不重复执行，
// 不保证 Up 内部语句幂等——中途失败时 DDL 已被 MySQL 隐式提交，重跑要靠 Up 自己的探测收敛。
type GoMigration struct {
	Version int
	Name    string
	Up      func(*gorm.DB) error
}

// migrationStep 把 SQL 脚本迁移与 Go 迁移统一成一种可执行形态，按 version 统一排序。
type migrationStep struct {
	version int
	name    string
	apply   func(*gorm.DB) error
}

// Migrate 按版本号升序执行尚未应用的迁移，返回本次新应用的条数。
//
// 幂等：连续执行两次，第二次返回 0 且不产生任何写入。
// 事务边界（诚实表述）：MySQL 的 DDL 会隐式提交，InnoDB 无法回滚建表/改表，
// 因此这里的事务只能保证「迁移记录与实际执行结果同生共死」，
// 语句层面的失败仍需靠脚本自身幂等（CREATE TABLE IF NOT EXISTS / Go 迁移的列探测）重跑才能收敛。
//
// 输出：每条实际应用的迁移都会打印 version+name。-migrate 的引导文案由 cmd 负责，
// 但「这次到底跑了哪几条」只有 runner 知道，所以打印留在这里，避免为一行日志改调用链。
func Migrate(db *gorm.DB) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("数据库连接未初始化")
	}
	if err := db.Exec(schemaMigrationsDDL).Error; err != nil {
		return 0, fmt.Errorf("创建迁移版本表 %s 失败: %w", schemaMigrationsTable, err)
	}

	steps, err := migrationSteps()
	if err != nil {
		return 0, fmt.Errorf("加载迁移失败: %w", err)
	}

	appliedVersions, err := appliedMigrationVersions(db)
	if err != nil {
		return 0, err
	}

	applied := 0
	for _, step := range steps {
		if _, done := appliedVersions[step.version]; done {
			continue
		}
		executed, err := applyStep(db, step)
		if err != nil {
			return applied, err
		}
		if !executed {
			// 并发执行时被其它进程抢先应用，本进程不重复执行。
			continue
		}
		fmt.Printf("  → 已应用迁移 %03d_%s\n", step.version, step.name)
		applied++
	}
	return applied, nil
}

// migrationSteps 合并 database/migrations 下的 SQL 脚本与本包注册的 Go 迁移。
// 两边的版本号必须互不重复：版本号同时决定执行顺序，而顺序本身是契约
// （orders 加列必须先于索引切换），重复会让顺序取决于实现细节。
func migrationSteps() ([]migrationStep, error) {
	sqlMigrations, err := database.Migrations()
	if err != nil {
		return nil, fmt.Errorf("加载迁移脚本失败: %w", err)
	}
	goMigrations := orderMigrations()

	steps := make([]migrationStep, 0, len(sqlMigrations)+len(goMigrations))
	seen := make(map[int]string, len(sqlMigrations)+len(goMigrations))

	add := func(version int, name, source string, apply func(*gorm.DB) error) error {
		if existing, duplicated := seen[version]; duplicated {
			return fmt.Errorf("迁移版本号重复：%s 与 %s（%s）都是 %d", existing, name, source, version)
		}
		seen[version] = fmt.Sprintf("%s（%s）", name, source)
		steps = append(steps, migrationStep{version: version, name: name, apply: apply})
		return nil
	}

	for _, migration := range sqlMigrations {
		migration := migration
		source := fmt.Sprintf("database/migrations/%03d_%s.sql", migration.Version, migration.Name)
		if err := add(migration.Version, migration.Name, source, sqlMigrationApply(migration)); err != nil {
			return nil, err
		}
	}
	for _, migration := range goMigrations {
		migration := migration
		source := fmt.Sprintf("internal/adapter/mysql Go 迁移 %03d_%s", migration.Version, migration.Name)
		if err := add(migration.Version, migration.Name, source, migration.Up); err != nil {
			return nil, err
		}
	}

	sort.SliceStable(steps, func(i, j int) bool {
		return steps[i].version < steps[j].version
	})
	return steps, nil
}

// sqlMigrationApply 把一个 SQL 脚本包装成可执行步骤：按 `;` 切分后逐条执行。
func sqlMigrationApply(migration database.Migration) func(*gorm.DB) error {
	return func(db *gorm.DB) error {
		statements := splitMigrationStatements(migration.SQL)
		if len(statements) == 0 {
			return fmt.Errorf("迁移 %03d_%s 没有任何可执行语句", migration.Version, migration.Name)
		}
		for _, statement := range statements {
			if err := db.Exec(statement).Error; err != nil {
				return fmt.Errorf("迁移 %03d_%s 执行失败 [%s]: %w", migration.Version, migration.Name, statement, err)
			}
		}
		return nil
	}
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

// applyStep 在单个事务内执行一条迁移并写入版本记录。
// 返回 false 表示该版本已被其它进程应用（事务内二次确认，避免重复执行）。
func applyStep(db *gorm.DB, step migrationStep) (bool, error) {
	executed := false
	err := db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&SchemaMigrationRow{}).Where("version = ?", step.version).Count(&count).Error; err != nil {
			return fmt.Errorf("查询迁移版本 %d 失败: %w", step.version, err)
		}
		if count > 0 {
			return nil
		}

		if err := step.apply(tx); err != nil {
			return err
		}

		record := SchemaMigrationRow{
			Version:   step.version,
			Name:      step.name,
			AppliedAt: time.Now().UTC(),
		}
		if err := tx.Create(&record).Error; err != nil {
			return fmt.Errorf("写入迁移版本 %d 失败: %w", step.version, err)
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
