package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"llmrouter/internal/domain"
)

// 异步写合并：未到窗口不落盘，Flush 立即落盘；配置仍同步落盘。
func TestAsyncHotWritesAndFlush(t *testing.T) {
	path := t.TempDir() + "/data.json"
	st, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.SetAsyncWrites(true)
	st.UpsertProvider(&domain.Provider{ID: "p1", Name: "P", BaseURL: "https://example.com/v1"})
	m := &domain.Model{ID: "m1", Name: "glm", ProviderID: "p1"}
	st.UpsertModel(m)

	// 配置变更同步可见（data.json 已写）
	reloaded, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Provider("p1"); !ok {
		t.Fatal("配置应在 save() 后立即可见")
	}

	// 热数据在窗口内不保证落盘
	st.AddCallUsage("s1", "auto", m, "k1", domain.CallSample{Usage: domain.Usage{Prompt: 3, Completion: 5}})
	st.Flush()

	reloaded, err = LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.SessionUsage) != 1 || len(reloaded.Usage) != 1 {
		t.Fatalf("Flush 后热数据应落盘: usage=%d session=%d", len(reloaded.Usage), len(reloaded.SessionUsage))
	}
	if reloaded.SessionUsage[0].CompletionTokens != 5 {
		t.Fatalf("会话用量不对: %#v", reloaded.SessionUsage[0])
	}
	_ = time.Second
}

// provider 名是直连模型对外名的前缀：加载旧数据与新建时都要规范化成
// 非空、不含 "/"、全局唯一。
func TestProviderNameNormalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	raw := `{
		"providers": [
			{"id":"p1","name":"","base_url":"http://a"},
			{"id":"p2","name":"a/b","base_url":"http://b"},
			{"id":"p3","name":"a-b","base_url":"http://c"}
		]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"p1": "p1", "p2": "a-b", "p3": "a-b (2)"}
	for id, name := range want {
		p, ok := st.Provider(id)
		if !ok {
			t.Fatalf("缺少 provider %s", id)
		}
		if p.Name != name {
			t.Errorf("provider %s 名 = %q, want %q", id, p.Name, name)
		}
	}

	// 新建时重名自动追加序号
	st.UpsertProvider(&domain.Provider{Name: "same", BaseURL: "http://d"})
	st.UpsertProvider(&domain.Provider{Name: "same", BaseURL: "http://e"})
	if got := st.Providers[len(st.Providers)-1].Name; got != "same (2)" {
		t.Errorf("重名 provider 应自动加序号, got %q", got)
	}
}
