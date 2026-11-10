package upstream

import "encoding/json"

// ExtractModelIDs 从 /models 响应中提取模型名，兼容三种格式：
// OpenAI 的 {"data":[{"id":...}]}、GLM/Anthropic 风格的 {"models":[{"slug":...}]}、纯字符串数组
func ExtractModelIDs(b []byte) []string {
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, m := range v.Data {
		add(m.ID)
	}
	for _, raw := range v.Models {
		var m struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &m) == nil {
			switch {
			case m.ID != "":
				add(m.ID)
				continue
			case m.Slug != "":
				add(m.Slug)
				continue
			case m.Name != "":
				add(m.Name)
				continue
			}
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			add(s)
		}
	}
	return ids
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
