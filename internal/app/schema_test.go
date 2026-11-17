package app

import (
	"reflect"
	"testing"
)

// TestMissingSchemaTables 锁定启动自检的判定：
// 缺哪张报哪张，顺序稳定（便于运维一眼看出缺的是哪一批），
// 全部存在时返回空切片而不是 nil（避免调用方 len(nil) 与 len(空) 行为差异）。
func TestMissingSchemaTables(t *testing.T) {
	allExists := func(string) bool { return true }
	if got := missingSchemaTables(allExists); len(got) != 0 {
		t.Fatalf("表齐全时不应报缺表，实际 %v", got)
	}

	// 现场场景：老库升级到 V3.6 但没跑 -migrate，merchants/merchant_keys 缺失。
	exists := func(table string) bool {
		return table != "merchants" && table != "merchant_keys"
	}
	want := []string{"merchants", "merchant_keys"}
	if got := missingSchemaTables(exists); !reflect.DeepEqual(got, want) {
		t.Fatalf("应报缺 %v，实际 %v", want, got)
	}

	nothing := func(string) bool { return false }
	if got := missingSchemaTables(nothing); len(got) != len(requiredSchemaTables) {
		t.Fatalf("空库应报全部 %d 张表，实际 %v", len(requiredSchemaTables), got)
	}
}

// TestEnsureSchemaRejectsNilDatabase 锁定空连接不当成「表齐全」。
func TestEnsureSchemaRejectsNilDatabase(t *testing.T) {
	if err := EnsureSchema(nil); err == nil {
		t.Fatal("nil 数据库必须报错，不能静默通过")
	}
}
