package store

import (
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
