package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 旧版（time.Time 直存 RFC3339）的 data.json 应能继续加载
func TestWBLegacyDataFileLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	old := `{"next_id":3,"providers":[{"id":"p1","name":"wb","base_url":"https://www.workbuddy.cn/v2","kind":"workbuddy",
		"wb":{"uid":"u","access_token":"at","refresh_token":"rt","expires_at":"2026-11-07T18:00:00+08:00"}}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStore(path)
	if err != nil {
		t.Fatalf("旧格式应能加载: %v", err)
	}
	p, ok := st.Provider("p1")
	if !ok || p.WB == nil || p.WB.ExpiresAt == nil {
		t.Fatalf("凭证应完整解析: %#v", p.WB)
	}
	want := time.Date(2026, 11, 7, 18, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if !p.WB.ExpiresAt.Equal(want) {
		t.Fatalf("过期时刻解析不对: %v", p.WB.ExpiresAt)
	}
	// 重新落盘后仍是合法 JSON，且不产生 0001-01-01
	st.Save()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded struct {
		Providers []struct {
			WB struct {
				ExpiresAt *time.Time `json:"expires_at"`
			} `json:"wb"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &reloaded); err != nil {
		t.Fatalf("回写结果应是合法 JSON: %v\n%s", err, b)
	}
	if got := reloaded.Providers[0].WB.ExpiresAt; got == nil || !got.Equal(want) {
		t.Fatalf("回写应保留原过期时刻: %v\n%s", got, b)
	}
}
