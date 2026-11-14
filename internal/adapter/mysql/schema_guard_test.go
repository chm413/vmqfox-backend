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

// findIllegalDefaults 找出「BLOB/TEXT/JSON/GEOMETRY 列带 DEFAULT」的定义。
// MySQL 5.7 明确禁止这四类列有默认值（ERROR 1101），而 compose 钉的是 5.7；
// 这类错误只在真正建表时暴露，单测跑不到，因此在这里做静态兜底。
//
// 按「列定义」而不是「行」匹配：仓库风格是一列一行，但只要有人把类型和 DEFAULT
// 拆成两行，按行扫就会漏报；反过来，默认值字符串里的 'text'/'blob' 又会被按行扫误报。
func findIllegalDefaults(content string) []string {
	var found []string
	for _, statement := range splitSQLStatements(stripSQLLineComments(content)) {
		for _, column := range splitColumnDefinitions(statement) {
			// 先剥离字符串字面量，避免 DEFAULT 'text' 里的 text 被当成列类型。
			stripped := stripStringLiterals(column)
			if forbiddenType.MatchString(stripped) && defaultClause.MatchString(stripped) {
				found = append(found, strings.TrimSpace(column))
			}
		}
	}
	return found
}

// splitColumnDefinitions 从 CREATE TABLE / ALTER TABLE 语句里切出列定义。
// CREATE TABLE 只看最外层括号内部，按深度 1 的逗号切分（DECIMAL(10,2) 这类括号不切）。
func splitColumnDefinitions(statement string) []string {
	trimmed := strings.TrimSpace(statement)
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "ALTER TABLE") {
		// ALTER TABLE ... ADD COLUMN x TEXT ... DEFAULT ...：整个 statement 就是一条列定义。
		if strings.Contains(upper, "ADD COLUMN") || strings.Contains(upper, "ADD ") {
			return []string{trimmed}
		}
		return nil
	}
	if !strings.HasPrefix(upper, "CREATE TABLE") {
		return nil
	}

	open := strings.Index(trimmed, "(")
	if open < 0 {
		return nil
	}

	var (
		columns []string
		current strings.Builder
		depth   int
		quote   byte
	)
	for index := open + 1; index < len(trimmed); index++ {
		char := trimmed[index]
		switch {
		case quote != 0:
			current.WriteByte(char)
			if char == quote {
				quote = 0
			}
			continue
		case char == '\'' || char == '"' || char == '`':
			quote = char
			current.WriteByte(char)
			continue
		case char == '(':
			depth++
		case char == ')':
			if depth == 0 {
				// 表定义结束
				if definition := strings.TrimSpace(current.String()); definition != "" {
					columns = append(columns, definition)
				}
				return columns
			}
			depth--
		case char == ',' && depth == 0:
			if definition := strings.TrimSpace(current.String()); definition != "" {
				columns = append(columns, definition)
			}
			current.Reset()
			continue
		}
		current.WriteByte(char)
	}
	return columns
}

// stripStringLiterals 去掉单引号/双引号字符串字面量，保留其余字符。
func stripStringLiterals(input string) string {
	var builder strings.Builder
	var quote byte
	for index := 0; index < len(input); index++ {
		char := input[index]
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '\'' || char == '"':
			quote = char
		default:
			builder.WriteByte(char)
		}
	}
	return builder.String()
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

// TestFindIllegalDefaultsDetectsRegression 证明守卫不是空转，并钉住几个边界。
func TestFindIllegalDefaultsDetectsRegression(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{
			name: "单行非法（当初的真实形态）",
			sql: "CREATE TABLE t (\n" +
				"  id BIGINT NOT NULL,\n" +
				"  peer_public_key TEXT NOT NULL DEFAULT '',\n" +
				"  PRIMARY KEY (id)\n" +
				");",
			want: 1,
		},
		{
			name: "类型与 DEFAULT 跨行（按行扫会漏报）",
			sql: "CREATE TABLE t (\n" +
				"  id BIGINT NOT NULL,\n" +
				"  peer_public_key TEXT NOT NULL\n" +
				"    DEFAULT '',\n" +
				"  PRIMARY KEY (id)\n" +
				");",
			want: 1,
		},
		{
			name: "反引号列名",
			sql: "CREATE TABLE t (\n" +
				"  `payload` LONGTEXT NOT NULL DEFAULT '',\n" +
				"  PRIMARY KEY (id)\n" +
				");",
			want: 1,
		},
		{
			name: "ALTER TABLE ADD COLUMN",
			sql:  "ALTER TABLE orders ADD COLUMN memo TEXT NOT NULL DEFAULT '';",
			want: 1,
		},
		{
			name: "默认值字符串里出现 text/blob（按行扫会误报）",
			sql: "CREATE TABLE t (\n" +
				"  media_type VARCHAR(16) NOT NULL DEFAULT 'text',\n" +
				"  kind VARCHAR(16) NOT NULL DEFAULT 'blob',\n" +
				"  PRIMARY KEY (id)\n" +
				");",
			want: 0,
		},
		{
			name: "合法定义 + 解释性注释",
			sql: "CREATE TABLE t (\n" +
				"  id BIGINT NOT NULL,\n" +
				"  -- 不能用 TEXT：5.7 禁止 TEXT 带 DEFAULT（这里只是注释）\n" +
				"  peer_public_key VARCHAR(1024) NOT NULL DEFAULT '',\n" +
				"  secret_cipher BLOB NOT NULL,\n" +
				"  amount DECIMAL(10,2) NOT NULL DEFAULT 0,\n" +
				"  PRIMARY KEY (id)\n" +
				");",
			want: 0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if found := findIllegalDefaults(testCase.sql); len(found) != testCase.want {
				t.Fatalf("期望命中 %d 处，实际 %d 处: %v", testCase.want, len(found), found)
			}
		})
	}
}
