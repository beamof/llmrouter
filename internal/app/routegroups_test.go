package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonReq 带 JSON Content-Type 的任意方法请求（PUT/DELETE 辅助）
func jsonReq(t *testing.T, app *httptest.Server, method, path, body string) (*http.Response, string) {
	t.Helper()
	return postJSONWithHeader(t, app, method, path, body, "Content-Type", "application/json")
}

func groupMemberPriority(t *testing.T, s *Store, groupID, modelID string) (int, bool) {
	t.Helper()
	g, ok := s.GroupByID(groupID)
	if !ok {
		return 0, false
	}
	for _, mem := range g.Members {
		if mem.ModelID == modelID {
			return mem.Priority, true
		}
	}
	return 0, false
}

// 旧状态文件没有 groups：按原 auto_model 建默认组，所有模型作为成员归入默认组。
func TestRouteGroupLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	legacy := `{
	  "next_id": 3,
	  "auto_model": "pick",
	  "providers": [{"id":"p1","name":"P","base_url":"http://up"}],
	  "models": [{"id":"m2","provider_id":"p1","name":"real","priority":5}],
	  "api_keys": [], "usage": [], "balances": {}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	groups := s.GroupsList()
	if len(groups) != 1 || groups[0].ID != defaultGroupID || groups[0].Name != "pick" {
		t.Fatalf("旧 auto_model 应迁移为默认组: %+v", groups)
	}
	if p, ok := groupMemberPriority(t, s, defaultGroupID, "m2"); !ok || p != 5 {
		t.Fatalf("旧模型应成为默认组成员且保留优先级: %d %v", p, ok)
	}
	m, _ := s.Model("m2")
	if m.GroupID != "" {
		t.Errorf("迁移后不应再保留单组字段, got %q", m.GroupID)
	}
	if m.Priority != 5 {
		t.Errorf("直连优先级 = %d, want 5", m.Priority)
	}
	if got := s.AutoModelName(); got != "pick" {
		t.Errorf("AutoModelName = %q, want pick", got)
	}
}

// 上一版「每模型单组」的数据（组 + model.group_id）要折叠成成员关系。
func TestRouteGroupMigrateSingleGroupData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.json")
	stale := `{
	  "next_id": 5,
	  "groups": [{"id":"default","name":"auto"},{"id":"g1","name":"coding"}],
	  "models": [
	    {"id":"m1","provider_id":"p1","name":"a","priority":3,"group_id":"default"},
	    {"id":"m2","provider_id":"p1","name":"b","priority":7,"group_id":"g1"}
	  ],
	  "providers": [{"id":"p1","name":"P","base_url":"http://up"}],
	  "api_keys": [], "usage": [], "balances": {}
	}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := groupMemberPriority(t, s, "default", "m1"); !ok || p != 3 {
		t.Fatalf("m1 应迁入 default 组: %d %v", p, ok)
	}
	if p, ok := groupMemberPriority(t, s, "g1", "m2"); !ok || p != 7 {
		t.Fatalf("m2 应迁入 g1 组: %d %v", p, ok)
	}
	for _, id := range []string{"m1", "m2"} {
		if m, _ := s.Model(id); m.GroupID != "" {
			t.Errorf("%s 迁移后 group_id 应清空: %q", id, m.GroupID)
		}
	}
}

// 同一个模型配置可加入多个组，各组只有优先级不同，配置本身共享。
func TestRouteGroupSharedModelMembership(t *testing.T) {
	s, app := newTestServer(t)
	p := &Provider{Name: "up", BaseURL: "http://up"}
	s.store.UpsertProvider(p)
	coding, err := s.store.AddGroup("coding", "")
	if err != nil {
		t.Fatal(err)
	}
	cheap, err := s.store.AddGroup("cheap", "")
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{Name: "gpt-4o", ProviderID: p.ID, Priority: 5, DailyLimit: 1000, MaxConcurrent: 2, Expose: true}
	if err := s.store.UpsertModelInGroup(m, coding.ID, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AttachModelToGroup(cheap.ID, m.ID, 1); err != nil {
		t.Fatal(err)
	}

	// 同一份配置、两个组各自优先级
	if got, _ := groupMemberPriority(t, s.store, coding.ID, m.ID); got != 5 {
		t.Errorf("coding 组优先级 = %d, want 5", got)
	}
	if got, _ := groupMemberPriority(t, s.store, cheap.ID, m.ID); got != 1 {
		t.Errorf("cheap 组优先级 = %d, want 1", got)
	}
	if len(s.store.Models) != 1 {
		t.Fatalf("共享模型不应复制配置: %d 条", len(s.store.Models))
	}
	got, _ := s.store.Model(m.ID)
	if got.DailyLimit != 1000 || got.MaxConcurrent != 2 || !got.Expose {
		t.Errorf("共享配置字段异常: %+v", got)
	}
	if got.Priority != 5 { // 直连优先级取各组最大值
		t.Errorf("直连优先级 = %d, want 5", got.Priority)
	}

	// 修改共享配置（含 coding 组优先级）不应影响 cheap 组优先级
	edit := got
	edit.DailyLimit = 2000
	edit.Priority = 3
	if err := s.store.UpsertModelInGroup(&edit, coding.ID, 3); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.store.Model(m.ID); g.DailyLimit != 2000 {
		t.Errorf("共享配置应更新: %+v", g)
	}
	if p, _ := groupMemberPriority(t, s.store, cheap.ID, m.ID); p != 1 {
		t.Errorf("cheap 组优先级不应被连带修改: %d", p)
	}

	// 从 coding 移除：模型仍属于 cheap，配置保留
	if resp, body := jsonReq(t, app, "DELETE", "/api/groups/"+coding.ID+"/models/"+m.ID, ""); resp.StatusCode != 200 {
		t.Fatalf("移出组失败: %d %s", resp.StatusCode, body)
	}
	if _, ok := s.store.Model(m.ID); !ok {
		t.Fatal("仍属于其他组的模型配置应保留")
	}
	// 再从 cheap 移除：已无归属，配置一并删除
	if resp, body := jsonReq(t, app, "DELETE", "/api/groups/"+cheap.ID+"/models/"+m.ID, ""); resp.StatusCode != 200 {
		t.Fatalf("移出最后组失败: %d %s", resp.StatusCode, body)
	}
	if _, ok := s.store.Model(m.ID); ok {
		t.Fatal("不再属于任何组的模型配置应删除")
	}
}

func TestRouteGroupCRUD(t *testing.T) {
	s, app := newTestServer(t)

	groups := s.store.GroupsList()
	if len(groups) != 1 || groups[0].Name != "auto" {
		t.Fatalf("默认组不正确: %+v", groups)
	}
	if resp, body := jsonReq(t, app, "DELETE", "/api/groups/"+groups[0].ID, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("删除唯一组应 400: %d %s", resp.StatusCode, body)
	}

	if resp, body := postJSON(t, app, "/api/groups", `{"name":"coding","note":"编码"}`); resp.StatusCode != 200 {
		t.Fatalf("新建组失败: %d %s", resp.StatusCode, body)
	}
	if resp, body := postJSON(t, app, "/api/groups", `{"name":"cheap"}`); resp.StatusCode != 200 {
		t.Fatalf("新建组失败: %d %s", resp.StatusCode, body)
	}
	if resp, _ := postJSON(t, app, "/api/groups", `{"name":"coding"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("重名组应 400, got %d", resp.StatusCode)
	}

	coding, ok := s.store.GroupByName("coding")
	if !ok {
		t.Fatal("coding 组不存在")
	}
	if resp, body := jsonReq(t, app, "PUT", "/api/groups/"+coding.ID, `{"name":"code","note":"新备注"}`); resp.StatusCode != 200 {
		t.Fatalf("重命名失败: %d %s", resp.StatusCode, body)
	}
	if g, _ := s.store.GroupByName("code"); g.Note != "新备注" {
		t.Fatalf("备注未保存: %+v", g)
	}
}

// 删除组只删「已不属于任何组」的模型；共享模型在其他组继续存在。
func TestRouteGroupDeleteKeepsSharedModels(t *testing.T) {
	s, app := newTestServer(t)
	p := &Provider{Name: "up", BaseURL: "http://up"}
	s.store.UpsertProvider(p)
	g1, _ := s.store.AddGroup("g1", "")
	g2, _ := s.store.AddGroup("g2", "")

	shared := &Model{Name: "shared", ProviderID: p.ID, Priority: 1}
	if err := s.store.UpsertModelInGroup(shared, g1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AttachModelToGroup(g2.ID, shared.ID, 5); err != nil {
		t.Fatal(err)
	}
	only := &Model{Name: "only", ProviderID: p.ID, Priority: 2}
	if err := s.store.UpsertModelInGroup(only, g1.ID, 2); err != nil {
		t.Fatal(err)
	}

	if resp, body := jsonReq(t, app, "DELETE", "/api/groups/"+g1.ID, ""); resp.StatusCode != 200 {
		t.Fatalf("删除组失败: %d %s", resp.StatusCode, body)
	}
	if _, ok := s.store.Model(shared.ID); !ok {
		t.Error("共享模型应保留")
	}
	if _, ok := s.store.Model(only.ID); ok {
		t.Error("仅属于被删组的模型应删除")
	}
	if p, _ := groupMemberPriority(t, s.store, g2.ID, shared.ID); p != 5 {
		t.Errorf("共享模型在 g2 的优先级应保留: %d", p)
	}
	// 直连优先级随 g1 成员关系消失而回落
	if m, _ := s.store.Model(shared.ID); m.Priority != 5 {
		t.Errorf("直连优先级 = %d, want 5", m.Priority)
	}
}

// 组间路由隔离，同一模型在两个组里可使用不同优先级。
func TestRouteGroupRoutingUsesMemberPriority(t *testing.T) {
	var hitA, hitB, hitShared int
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitA++
		fmt.Fprint(w, `{"ok":"A"}`)
	}))
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitB++
		fmt.Fprint(w, `{"ok":"B"}`)
	}))
	upS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitShared++
		fmt.Fprint(w, `{"ok":"S"}`)
	}))
	defer upA.Close()
	defer upB.Close()
	defer upS.Close()

	s, app := newTestServer(t)
	pA, pB, pS := &Provider{Name: "A", BaseURL: upA.URL}, &Provider{Name: "B", BaseURL: upB.URL}, &Provider{Name: "S", BaseURL: upS.URL}
	s.store.UpsertProvider(pA)
	s.store.UpsertProvider(pB)
	s.store.UpsertProvider(pS)
	groupA, _ := s.store.AddGroup("group-a", "")
	groupB, _ := s.store.AddGroup("group-b", "")

	// shared 模型同时属于 A/B；在 A 组优先级低，在 B 组优先级高
	shared := &Model{Name: "shared", ProviderID: pS.ID}
	if err := s.store.UpsertModelInGroup(shared, groupA.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AttachModelToGroup(groupB.ID, shared.ID, 99); err != nil {
		t.Fatal(err)
	}
	aOnly := &Model{Name: "model-a", ProviderID: pA.ID}
	if err := s.store.UpsertModelInGroup(aOnly, groupA.ID, 5); err != nil {
		t.Fatal(err)
	}
	bOnly := &Model{Name: "model-b", ProviderID: pB.ID}
	if err := s.store.UpsertModelInGroup(bOnly, groupB.ID, 2); err != nil {
		t.Fatal(err)
	}

	if _, body := postJSON(t, app, "/v1/chat/completions", `{"model":"group-a"}`); body != `{"ok":"A"}` {
		t.Fatalf("group-a 应选 A-only（组内 5 > shared 1）: %s", body)
	}
	if _, body := postJSON(t, app, "/v1/chat/completions", `{"model":"group-b"}`); body != `{"ok":"S"}` {
		t.Fatalf("group-b 应选 shared（组内 99 > B-only 2）: %s", body)
	}
	if hitShared != 1 || hitA != 1 || hitB != 0 {
		t.Errorf("路由命中次数不符: shared=%d A=%d B=%d", hitShared, hitA, hitB)
	}

	// /v1/models 暴露全部组名
	r, err := app.Client().Get(app.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	for _, want := range []string{`"id":"group-a"`, `"id":"group-b"`, `"id":"auto"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("/v1/models 应包含 %s: %s", want, b)
		}
	}
}

// 并行上限与每日用量属于共享模型配置：多个组共用同一份计数。
func TestRouteGroupRuntimeConfigShared(t *testing.T) {
	s, _ := newTestServer(t)
	p := &Provider{Name: "up", BaseURL: "http://up"}
	s.store.UpsertProvider(p)
	g1, _ := s.store.AddGroup("g1", "")
	g2, _ := s.store.AddGroup("g2", "")
	m := &Model{Name: "m", ProviderID: p.ID, MaxConcurrent: 1, DailyLimit: 100}
	if err := s.store.UpsertModelInGroup(m, g1.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AttachModelToGroup(g2.ID, m.ID, 1); err != nil {
		t.Fatal(err)
	}

	// 并行名额只有一个，g1 占满后 g2 也应看到并行已满
	if !s.store.TryAcquire(m.ID) {
		t.Fatal("首个名额应能获取")
	}
	for _, gid := range []string{g1.ID, g2.ID} {
		cands, skips := s.store.GroupCandidatesDetailed(gid, time.Now())
		if len(cands) != 0 || len(skips) != 1 || !strings.Contains(skips[0].Reason, "并行已满") {
			t.Errorf("%s 应因共享并行上限被剔除: cands=%d skips=%v", gid, len(cands), skips)
		}
	}
	s.store.Release(m.ID)

	// 今日用量也按模型配置共享：任一组打满额度，另一组同样超限
	s.store.AddUsage(m, "", Usage{Prompt: 60, Completion: 50})
	for _, gid := range []string{g1.ID, g2.ID} {
		cands, skips := s.store.GroupCandidatesDetailed(gid, time.Now())
		if len(cands) != 0 || len(skips) != 1 || !strings.Contains(skips[0].Reason, "每日 token 上限") {
			t.Errorf("%s 应因共享每日上限被剔除: cands=%d skips=%v", gid, len(cands), skips)
		}
	}
}

// 组内重排只改该组成员优先级；直连优先级取各组成员优先级最大值。
func TestRouteGroupReorderPriority(t *testing.T) {
	s, app := newTestServer(t)
	p := &Provider{Name: "up", BaseURL: "http://up"}
	s.store.UpsertProvider(p)
	g, _ := s.store.AddGroup("g", "")
	m1 := &Model{Name: "same", ProviderID: p.ID}
	m2 := &Model{Name: "same", ProviderID: p.ID}
	if err := s.store.UpsertModelInGroup(m1, g.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertModelInGroup(m2, g.ID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Post(app.URL+"/api/models/reorder", "application/json",
		strings.NewReader(`{"group_id":"`+g.ID+`","ids":["`+m1.ID+`","`+m2.ID+`"]}`)); err != nil {
		t.Fatal(err)
	}
	if p, _ := groupMemberPriority(t, s.store, g.ID, m1.ID); p != 2 {
		t.Errorf("m1 组内优先级 = %d, want 2", p)
	}
	if p, _ := groupMemberPriority(t, s.store, g.ID, m2.ID); p != 1 {
		t.Errorf("m2 组内优先级 = %d, want 1", p)
	}
	cands := s.store.Candidates("same", time.Now())
	if len(cands) != 2 || cands[0].Model.ID != m1.ID || cands[0].Model.Priority != 2 {
		t.Errorf("直连候选排序应跟随成员优先级: %+v", cands)
	}
}

// 加入已有模型 API：只建成员关系，不复制配置。
func TestRouteGroupAttachExistingModelAPI(t *testing.T) {
	s, app := newTestServer(t)
	p := &Provider{Name: "up", BaseURL: "http://up"}
	s.store.UpsertProvider(p)
	g1, _ := s.store.AddGroup("g1", "")
	g2, _ := s.store.AddGroup("g2", "")
	m := &Model{Name: "m", ProviderID: p.ID, DailyLimit: 500}
	if err := s.store.UpsertModelInGroup(m, g1.ID, 3); err != nil {
		t.Fatal(err)
	}
	if resp, body := postJSON(t, app, "/api/groups/"+g2.ID+"/models", `{"model_id":"`+m.ID+`","priority":8}`); resp.StatusCode != 200 {
		t.Fatalf("加入已有模型失败: %d %s", resp.StatusCode, body)
	}
	if len(s.store.Models) != 1 {
		t.Fatalf("不应复制模型配置: %d", len(s.store.Models))
	}
	if p, ok := groupMemberPriority(t, s.store, g2.ID, m.ID); !ok || p != 8 {
		t.Fatalf("g2 成员优先级 = %d %v, want 8", p, ok)
	}
	if mm, _ := s.store.Model(m.ID); mm.DailyLimit != 500 {
		t.Errorf("共享配置应保持不变: %+v", mm)
	}
}
