package mysql

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hulisang/vmqfox-backend/database"
)

// forbiddenType 匹配 5.7 里禁止带 DEFAULT 的列类型。
var forbiddenType = regexp.MustCompile(`(?i)\b(?:TINY|MEDIUM|LONG)?(?:TEXT|BLOB)\b|\bJSON\b|\bGEOMETRY\b`)

// defaultClause 匹配同一列定义里的 DEFAULT 关键字。
var defaultClause = regexp.MustCompile(`(?i)\bDEFAULT\b`)

// findIllegalDefaults 找出「BLOB/TEXT/JSON/GEOMETRY 列带 DEFAULT」的定义行。
// MySQL 5.7 明确禁止这四类列有默认值（ERROR 1101），而 compose 钉的是 5.7；
// 这类错误只在真正建表时暴露，单测跑不到，因此在这里做静态兜底。
func findIllegalDefaults(content string) []string {
	var found []string
	for _, statement := range splitSQLStatements(stripSQLLineComments(content)) {
		trimmed := strings.TrimSpace(statement)
		if trimmed == "" {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if !strings.HasPrefix(upper, "CREATE TABLE") && !strings.HasPrefix(upper, "ALTER TABLE") {
			continue
		}
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if forbiddenType.MatchString(line) && defaultClause.MatchString(line) {
				found = append(found, line)
			}
		}
	}
	return found
}

// stripSQLLineComments 去掉 `--` 行注释，避免注释里出现的关键字被误判。
func stripSQLLineComments(content string) string {
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if position := strings.Index(line, "--"); position >= 0 {
			lines[index] = line[:position]
		}
	}
	return strings.Join(lines, "\n")
}

func TestSchemaNoTextDefault(t *testing.T) {
	cases := map[string]string{"internal/adapter/mysql/schema.sql": schemaSQL}

	// database/schema.sql 没有被 go:embed 引用，但 docker 首次初始化直接挂载执行它，
	// 是"最先炸"的那条路径，必须一起守。
	reference, err := os.ReadFile("../../../database/schema.sql")
	if err != nil {
		t.Fatalf("读取 database/schema.sql 失败: %v", err)
	}
	cases["database/schema.sql"] = string(reference)

	migrations, err := database.Migrations()
	if err != nil {
		t.Fatalf("读取迁移脚本失败: %v", err)
	}
	for _, migration := range migrations {
		cases["database/migrations/"+migration.Name+".sql"] = migration.SQL
	}

	for name, content := range cases {
		for _, line := range findIllegalDefaults(content) {
			t.Errorf("%s 存在非法列定义（MySQL 5.7 禁止 BLOB/TEXT/JSON 列带 DEFAULT）: %s", name, line)
		}
	}
}

// TestFindIllegalDefaultsDetectsRegression 证明守卫本身有效：
// 曾经把 peer_public_key 写成 TEXT 且带空串默认值，在 5.7 上建表直接 ERROR 1101。
func TestFindIllegalDefaultsDetectsRegression(t *testing.T) {
	bad := "CREATE TABLE t (\n" +
		"  id BIGINT NOT NULL,\n" +
		"  peer_public_key TEXT NOT NULL DEFAULT '',\n" +
		"  PRIMARY KEY (id)\n" +
		");"
	if found := findIllegalDefaults(bad); len(found) != 1 {
		t.Fatalf("应识别出 1 处非法定义，实际 %d: %v", len(found), found)
	}

	good := "CREATE TABLE t (\n" +
		"  id BIGINT NOT NULL,\n" +
		"  -- 不能用 TEXT：5.7 禁止 TEXT 带 DEFAULT（这里只是注释）\n" +
		"  peer_public_key VARCHAR(1024) NOT NULL DEFAULT '',\n" +
		"  secret_cipher BLOB NOT NULL,\n" +
		"  PRIMARY KEY (id)\n" +
		");"
	if found := findIllegalDefaults(good); len(found) != 0 {
		t.Fatalf("合法定义被误判: %v", found)
	}
}
