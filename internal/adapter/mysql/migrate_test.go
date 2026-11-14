package mysql

import (
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/database"
	"gorm.io/gorm"
)

// resetMigrationState 清掉本次迁移涉及的表，让每个用例从「空库」开始。
// 只动 merchants / merchant_keys / schema_migrations，不碰既有业务表。
func resetMigrationState(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{"merchant_keys", "merchants", schemaMigrationsTable} {
		if err := db.Exec("DROP TABLE IF EXISTS " + table).Error; err != nil {
			t.Fatalf("清理表 %s 失败: %v", table, err)
		}
	}
}

func migrationTableExists(t *testing.T, db *gorm.DB, table string) bool {
	t.Helper()
	var count int64
	err := db.Raw(`
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&count).Error
	if err != nil {
		t.Fatalf("查询表 %s 是否存在失败: %v", table, err)
	}
	return count == 1
}

func merchantKeyPrimaryKey(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var columns []string
	err := db.Raw(`
		SELECT column_name
		FROM information_schema.key_column_usage
		WHERE table_schema = DATABASE()
		  AND table_name = 'merchant_keys'
		  AND constraint_name = 'PRIMARY'
		ORDER BY ordinal_position`).Scan(&columns).Error
	if err != nil {
		t.Fatalf("读取 merchant_keys 主键失败: %v", err)
	}
	result := ""
	for index, column := range columns {
		if index > 0 {
			result += ","
		}
		result += column
	}
	return result
}

// TestMigrationAppliesAndIsIdempotent 覆盖「首次应用建表」与「重复执行无副作用」。
func TestMigrationAppliesAndIsIdempotent(t *testing.T) {
	db := openTestDatabase(t)
	resetMigrationState(t, db)

	migrations, err := database.Migrations()
	if err != nil {
		t.Fatalf("解析迁移脚本失败: %v", err)
	}

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("首次迁移应成功，实际 err=%v", err)
	}
	if applied != len(migrations) {
		t.Fatalf("首次迁移应应用 %d 条，实际 %d 条", len(migrations), applied)
	}
	for _, table := range []string{"merchants", "merchant_keys", schemaMigrationsTable} {
		if !migrationTableExists(t, db, table) {
			t.Fatalf("迁移后应存在表 %s", table)
		}
	}
	// 主键顺序决定索引最左前缀，写错会导致按 merchant_id 的单条件查询全表扫描。
	if primaryKey := merchantKeyPrimaryKey(t, db); primaryKey != "merchant_id,protocol,generation" {
		t.Fatalf("merchant_keys 主键应为 (merchant_id,protocol,generation)，实际 %s", primaryKey)
	}

	var recorded int64
	if err := db.Model(&SchemaMigrationRow{}).Count(&recorded).Error; err != nil {
		t.Fatalf("读取版本记录失败: %v", err)
	}
	if recorded != int64(len(migrations)) {
		t.Fatalf("版本记录应为 %d 条，实际 %d 条", len(migrations), recorded)
	}

	applied, err = Migrate(db)
	if err != nil {
		t.Fatalf("重复迁移应成功，实际 err=%v", err)
	}
	if applied != 0 {
		t.Fatalf("重复迁移不应再应用任何迁移，实际 %d 条", applied)
	}
}

// TestMigrationSkipsAppliedVersion 覆盖「版本已记录但表被手工删掉」的运维误操作场景：
// 迁移器只认版本表，不重复执行——这正是需要人工介入的信号，不应被静默修复。
func TestMigrationSkipsAppliedVersion(t *testing.T) {
	db := openTestDatabase(t)
	resetMigrationState(t, db)

	migrations, err := database.Migrations()
	if err != nil {
		t.Fatalf("解析迁移脚本失败: %v", err)
	}
	if err := db.Exec(schemaMigrationsDDL).Error; err != nil {
		t.Fatalf("创建版本表失败: %v", err)
	}
	// 先把全部版本标记为已应用，模拟「版本表完整但表被手工删除」。
	for _, migration := range migrations {
		record := SchemaMigrationRow{Version: migration.Version, Name: migration.Name, AppliedAt: time.Now().UTC()}
		if err := db.Create(&record).Error; err != nil {
			t.Fatalf("写入版本记录 %d 失败: %v", migration.Version, err)
		}
	}

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("迁移应成功，实际 err=%v", err)
	}
	if applied != 0 {
		t.Fatalf("已记录的版本不应重复执行，实际应用 %d 条", applied)
	}
	if migrationTableExists(t, db, "merchants") {
		t.Fatal("已记录版本时迁移器不会重建表，此处出现 merchants 说明逻辑被改动")
	}
}
