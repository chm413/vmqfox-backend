package database

import (
	"strings"
	"testing"
)

// TestMigrationsParsedAndSorted 锁定嵌入脚本的解析结果：
// 版本号来自文件名前缀、名称来自主干、顺序按版本号升序。
func TestMigrationsParsedAndSorted(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("解析迁移脚本失败: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("至少要有一个迁移脚本，否则 -migrate 等于空转")
	}
	for index, migration := range migrations {
		if migration.Version <= 0 {
			t.Fatalf("迁移版本号必须为正数: %+v", migration)
		}
		if migration.Name == "" {
			t.Fatalf("迁移名称不能为空: %+v", migration)
		}
		if strings.TrimSpace(migration.SQL) == "" {
			t.Fatalf("迁移 %03d_%s 的脚本内容为空", migration.Version, migration.Name)
		}
		if index > 0 && migrations[index-1].Version >= migration.Version {
			t.Fatalf("迁移必须按版本号严格升序: %d 之后出现 %d",
				migrations[index-1].Version, migration.Version)
		}
	}

	first := migrations[0]
	if first.Version != 1 || first.Name != "merchants" {
		t.Fatalf("001_merchants.sql 应解析为 version=1 name=merchants，实际 version=%d name=%s",
			first.Version, first.Name)
	}
	// 迁移脚本必须自身幂等（MySQL DDL 不可回滚，失败只能重跑），这里锁定写法而非内容。
	if !strings.Contains(first.SQL, "CREATE TABLE IF NOT EXISTS merchants") {
		t.Fatal("001_merchants.sql 必须使用 CREATE TABLE IF NOT EXISTS merchants")
	}
	if !strings.Contains(first.SQL, "CREATE TABLE IF NOT EXISTS merchant_keys") {
		t.Fatal("001_merchants.sql 必须使用 CREATE TABLE IF NOT EXISTS merchant_keys")
	}
}

// TestParseMigrationFileName 覆盖合规与不合规文件名。
func TestParseMigrationFileName(t *testing.T) {
	version, name, err := parseMigrationFileName("007_orders_merchant.sql")
	if err != nil {
		t.Fatalf("合规文件名应解析成功，实际 err=%v", err)
	}
	if version != 7 || name != "orders_merchant" {
		t.Fatalf("解析结果错误: version=%d name=%s", version, name)
	}

	invalid := []string{
		"merchants.sql",       // 没有版本前缀
		"001merchants.sql",    // 缺少下划线分隔
		"1_merchants.sql",     // 前缀不是三位
		"001_merchants.txt",   // 扩展名不对
		"000_merchants.sql",   // 版本号必须大于 0
		"001_Merchants.sql",   // 描述部分必须小写
		"001_merchants-2.sql", // 描述部分不允许连字符
		"001_.sql",            // 描述部分为空
	}
	for _, fileName := range invalid {
		if _, _, err := parseMigrationFileName(fileName); err == nil {
			t.Fatalf("不合规文件名应报错: %s", fileName)
		}
	}
}
