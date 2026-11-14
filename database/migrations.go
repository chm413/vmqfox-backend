// Package database 承载数据库侧的静态资源与迁移脚本注册表。
//
// 迁移脚本通过 go:embed 编进二进制，部署时不需要额外的文件挂载；
// 执行入口是 vmqfox-api -migrate（启动时不会自动迁移，与现有「启动不建表」策略一致）。
package database

import (
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationsDir 是嵌入文件系统内的脚本目录。
const migrationsDir = "migrations"

// Migration 表示一个已发布的数据库迁移。
// Version 来自文件名前缀数字（001_merchants.sql → 1），Name 来自前缀之后的主干（merchants）。
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations 读取全部嵌入的迁移脚本，按 Version 升序返回。
// 文件名不合规、版本号重复或目录内混入非 .sql 文件时一律报错——
// 静默跳过会让「脚本存在但没执行」这种事故无法被发现。
func Migrations() ([]Migration, error) {
	entries, err := migrationFiles.ReadDir(migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("读取嵌入迁移目录失败: %w", err)
	}

	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		version, name, err := parseMigrationFileName(fileName)
		if err != nil {
			return nil, err
		}
		if existing, duplicated := seen[version]; duplicated {
			return nil, fmt.Errorf("迁移版本号重复: %s 与 %s 都是 %d", existing, fileName, version)
		}
		content, err := migrationFiles.ReadFile(migrationsDir + "/" + fileName)
		if err != nil {
			return nil, fmt.Errorf("读取迁移脚本 %s 失败: %w", fileName, err)
		}
		seen[version] = fileName
		migrations = append(migrations, Migration{Version: version, Name: name, SQL: string(content)})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

// parseMigrationFileName 解析 `001_merchants.sql` 形式的文件名。
// 前缀固定三位数字，保证目录的字典序与数值序一致，人工排查时不会误判先后。
func parseMigrationFileName(fileName string) (int, string, error) {
	const extension = ".sql"
	if !strings.HasSuffix(fileName, extension) {
		return 0, "", fmt.Errorf("迁移文件名必须以 %s 结尾: %s", extension, fileName)
	}
	base := strings.TrimSuffix(fileName, extension)

	prefix, name, found := strings.Cut(base, "_")
	if !found {
		return 0, "", fmt.Errorf("迁移文件名必须形如 001_merchants.sql: %s", fileName)
	}
	if len(prefix) != 3 {
		return 0, "", fmt.Errorf("迁移文件名前缀必须是三位数字（如 001）: %s", fileName)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("迁移文件名前缀必须是正整数版本号: %s", fileName)
	}
	if name == "" {
		return 0, "", fmt.Errorf("迁移文件名缺少描述部分: %s", fileName)
	}
	for _, char := range name {
		isLower := char >= 'a' && char <= 'z'
		isDigit := char >= '0' && char <= '9'
		if !isLower && !isDigit && char != '_' {
			return 0, "", fmt.Errorf("迁移文件名描述部分只允许小写字母、数字与下划线: %s", fileName)
		}
	}
	return version, name, nil
}
