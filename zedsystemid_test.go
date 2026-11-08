package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZedNormalizeSystemID(t *testing.T) {
	ok := "0b65f4f3-1a2b-4c3d-9e8f-7a6b5c4d3e2f"
	if got, err := zedNormalizeSystemID(" " + strings.ToUpper(ok) + " "); err != nil || got != ok {
		t.Fatalf("合法 UUID 应规范化成功: %q %v", got, err)
	}
	if got, err := zedNormalizeSystemID(""); err != nil || got != "" {
		t.Fatalf("空值应返回空串: %q %v", got, err)
	}
	for _, bad := range []string{
		"not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"0b65f4f3-1a2b-4c3d-9e8f-7a6b5c4d3e2", "0b65f4f3_1a2b-4c3d-9e8f-7a6b5c4d3e2f",
	} {
		if _, err := zedNormalizeSystemID(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
}

// 用真实 SQLite 文件（python sqlite3 生成的 fixture）验证最小解析器
func TestZedSQLiteKV(t *testing.T) {
	id, err := zedSQLiteKV(filepath.Join("testdata", "zed-db.sqlite"), "system_id")
	if err != nil {
		t.Fatalf("读取 fixture 失败: %v", err)
	}
	want := "0b65f4f3-1a2b-4c3d-9e8f-7a6b5c4d3e2f"
	if id != want {
		t.Fatalf("system_id = %q, want %q", id, want)
	}
	// 不存在的键返回空串
	if v, err := zedSQLiteKV(filepath.Join("testdata", "zed-db.sqlite"), "nope"); err != nil || v != "" {
		t.Fatalf("不存在的键应为空串: %q %v", v, err)
	}
}

func TestZedSQLiteKVCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	os.WriteFile(path, []byte("this is not a sqlite database at all.............."), 0o600)
	if _, err := zedSQLiteKV(path, "system_id"); err == nil {
		t.Fatal("损坏文件应报错而不是 panic")
	}
	// 缺表：合法 SQLite 头但没有 kv_store 表。构造一个假头（页大小 4096）
	hdr := make([]byte, 4096)
	copy(hdr, "SQLite format 3\x00")
	hdr[16], hdr[17] = 0x10, 0x00 // page size 4096
	os.WriteFile(path, hdr, 0o600)
	if _, err := zedSQLiteKV(path, "system_id"); err == nil {
		t.Fatal("缺表应报错")
	}
}
