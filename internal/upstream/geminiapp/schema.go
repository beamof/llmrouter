package geminiapp

// 有界的 JSON Schema 校验（工具实参 / 结构化输出共用）。支持常用的断言关键字；
// 不认识的关键字忽略（宁可少校验也不误拒合法的调用），绝不执行正则，也不抓取
// 外部引用——只认 schema 内 $defs / definitions 下的本地指针。

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxSchemaDepth 值与 schema 的最大嵌套层数
const maxSchemaDepth = 16

// wildcardPatterns 本地校验唯一认可的 patternProperties 模式（即「任意键名」）
var wildcardPatterns = []string{"^(.*)$", "^.*$"}

// schemaMatches 值是否满足 schema。root 是整份 schema（解析本地 $ref 用）。
func schemaMatches(value, schema, root any, depth int) bool {
	if depth > maxSchemaDepth {
		return false
	}
	s, ok := schema.(map[string]any)
	if !ok {
		if b, isBool := schema.(bool); isBool {
			return b
		}
		return true // null 或不认识的形态：不校验
	}
	if value == nil {
		if nullable, _ := s["nullable"].(bool); nullable {
			return true
		}
	}
	if ref, ok := s["$ref"].(string); ok {
		if target, ok := resolveRef(root, ref); ok {
			if !schemaMatches(value, target, root, depth+1) {
				return false
			}
		}
	}
	if lit, has := s["const"]; has && !jsonEqual(value, lit) {
		return false
	}
	if list, ok := s["enum"].([]any); ok {
		hit := false
		for _, x := range list {
			if hit = jsonEqual(value, x); hit {
				break
			}
		}
		if !hit {
			return false
		}
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, x := range all {
			if !schemaMatches(value, x, root, depth+1) {
				return false
			}
		}
	}
	if not, has := s["not"]; has && not != nil && schemaMatches(value, not, root, depth+1) {
		return false
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		list, ok := s[key].([]any)
		if !ok || len(list) == 0 {
			continue
		}
		n := 0
		for _, x := range list {
			if schemaMatches(value, x, root, depth+1) {
				if n++; key == "anyOf" {
					break
				}
			}
		}
		if key == "anyOf" {
			if n < 1 {
				return false
			}
		} else if n != 1 {
			return false
		}
	}
	types := schemaTypes(s)
	if len(types) == 0 { // 没写 type：带对象类关键字时按对象校验
		for _, k := range []string{"properties", "required", "additionalProperties", "patternProperties"} {
			if _, has := s[k]; has {
				types = []string{"object"}
				break
			}
		}
	}
	if len(types) > 0 && !typeMatchesAny(value, types) {
		return false
	}
	switch v := value.(type) {
	case string:
		n := utf8.RuneCountInString(v)
		if min, ok := schemaInt(s, "minLength"); ok && n < min {
			return false
		}
		if max, ok := schemaInt(s, "maxLength"); ok && n > max {
			return false
		}
		return true
	case []any:
		if min, ok := schemaInt(s, "minItems"); ok && len(v) < min {
			return false
		}
		if max, ok := schemaInt(s, "maxItems"); ok && len(v) > max {
			return false
		}
		if uniq, _ := s["uniqueItems"].(bool); uniq {
			seen := map[string]bool{}
			for _, item := range v {
				k := canonicalJSON(item)
				if seen[k] {
					return false
				}
				seen[k] = true
			}
		}
		var prefix []any
		rest, hasRest := s["items"]
		if pi, ok := s["prefixItems"].([]any); ok {
			prefix = pi
		} else if tuple, ok := s["items"].([]any); ok {
			prefix, rest, hasRest = tuple, s["additionalItems"], true
		}
		for i := 0; i < len(v) && i < len(prefix); i++ {
			if !schemaMatches(v[i], prefix[i], root, depth+1) {
				return false
			}
		}
		if hasRest && rest != nil {
			for i := len(prefix); i < len(v); i++ {
				if !schemaMatches(v[i], rest, root, depth+1) {
					return false
				}
			}
		}
		return true
	case map[string]any:
		if min, ok := schemaInt(s, "minProperties"); ok && len(v) < min {
			return false
		}
		if max, ok := schemaInt(s, "maxProperties"); ok && len(v) > max {
			return false
		}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok {
					if _, has := v[name]; !has {
						return false
					}
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		patterns, _ := s["patternProperties"].(map[string]any)
		additional, hasAdditional := s["additionalProperties"]
		for k, x := range v {
			covered := false
			if ps, ok := props[k]; ok {
				if !schemaMatches(x, ps, root, depth+1) {
					return false
				}
				covered = true
			}
			for pattern, ps := range patterns {
				if covered || !contains(wildcardPatterns, pattern) {
					continue
				}
				if !schemaMatches(x, ps, root, depth+1) {
					return false
				}
				covered = true
			}
			if !covered && hasAdditional && additional != nil && !schemaMatches(x, additional, root, depth+1) {
				return false
			}
		}
		return true
	}
	return schemaNumberMatches(value, s)
}

// schemaNumberMatches 数值的区间与倍数约束（非数值直接通过，类型已由 type 校验过）
func schemaNumberMatches(value any, s map[string]any) bool {
	vr, ok := numberRat(value)
	if !ok {
		return true
	}
	within := func(key string, less bool) bool { // 是否落在 [bound, +∞) 或 (-∞, bound] 内
		bound, ok := numberRat(s[key])
		if !ok {
			return true
		}
		c := vr.Cmp(bound)
		return !(less && c < 0) && !(!less && c > 0)
	}
	// draft-04 写法：exclusiveMinimum / exclusiveMaximum 是布尔开关，配合 minimum / maximum
	if !within("minimum", true) || !within("maximum", false) {
		return false
	}
	if excl, _ := s["exclusiveMinimum"].(bool); excl && !strictlyBeyond(vr, s["minimum"], true) {
		return false
	}
	if excl, _ := s["exclusiveMaximum"].(bool); excl && !strictlyBeyond(vr, s["maximum"], false) {
		return false
	}
	if !strictlyBeyond(vr, s["exclusiveMinimum"], true) || !strictlyBeyond(vr, s["exclusiveMaximum"], false) {
		return false
	}
	if mr, ok := numberRat(s["multipleOf"]); ok && mr.Sign() > 0 {
		if !new(big.Rat).Quo(vr, mr).IsInt() {
			return false
		}
	}
	return true
}

// strictlyBeyond 值是否严格大于（above）或严格小于（not above）边界；边界缺失或不是数字时不约束
func strictlyBeyond(vr *big.Rat, bound any, above bool) bool {
	br, ok := numberRat(bound)
	if !ok {
		return true
	}
	c := vr.Cmp(br)
	if above {
		return c > 0
	}
	return c < 0
}

// schemaTypes type 关键字的类型名列表（不认识的类型名忽略）
func schemaTypes(s map[string]any) []string {
	var out []string
	switch t := s["type"].(type) {
	case string:
		out = append(out, t)
	case []any:
		for _, x := range t {
			if name, ok := x.(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func typeMatchesAny(value any, types []string) bool {
	for _, t := range types {
		switch t {
		case "null":
			if value == nil {
				return true
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "number":
			if _, ok := value.(json.Number); ok {
				return true
			}
		case "integer":
			if _, ok := value.(json.Number); ok && isIntegerNumber(value) {
				return true
			}
		case "array":
			if _, ok := value.([]any); ok {
				return true
			}
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// schemaInt 非负整数关键字
func schemaInt(s map[string]any, key string) (int, bool) {
	f, ok := s[key].(float64)
	if !ok || f < 0 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// resolveRef 解析 schema 内的本地指针（必须落在 $defs / definitions 下）
func resolveRef(root any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	segs := strings.Split(ref[2:], "/")
	if segs[0] != "$defs" && segs[0] != "definitions" {
		return nil, false
	}
	cur := root
	for _, seg := range segs {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			if seg == "" || (len(seg) > 1 && seg[0] == '0') {
				return nil, false
			}
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// ── 结构化输出 ──────────────────────────────────────────────────────────────

// stripSoleTopLevelJSONFence 正文恰好是一个 ```json / ``` 围栏时去掉围栏
func stripSoleTopLevelJSONFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") || len(trimmed) < 6 {
		return trimmed
	}
	firstNl := strings.IndexByte(trimmed, '\n')
	if firstNl < 0 {
		return trimmed
	}
	header := strings.TrimSuffix(trimmed[3:firstNl], "\r")
	if header != "" && header != "json" {
		return trimmed
	}
	beforeClosing := trimmed[firstNl+1 : len(trimmed)-3]
	if !strings.HasSuffix(beforeClosing, "\n") {
		return trimmed
	}
	inner := strings.TrimSuffix(beforeClosing, "\r\n")
	if len(inner) == len(beforeClosing) {
		inner = beforeClosing[:len(beforeClosing)-1]
	}
	return strings.TrimSpace(inner)
}

// validateStructuredOutput 最终正文是否满足 response_format；返回去掉围栏后的 JSON 文本
func validateStructuredOutput(content string, rf *responseFormat) (string, error) {
	candidate := stripSoleTopLevelJSONFence(content)
	v, err := decodeJSONNumber([]byte(candidate))
	if err != nil {
		return "", upstreamFailure("INVALID_STRUCTURED_OUTPUT_JSON", 502, "模型输出不是合法的 JSON（response_format 要求结构化输出）")
	}
	if rf.kind == "json_object" {
		if _, ok := v.(map[string]any); !ok {
			return "", upstreamFailure("STRUCTURED_OUTPUT_SCHEMA_MISMATCH", 502, "模型输出不是 JSON 对象（response_format 要求单个 JSON 对象）")
		}
	}
	if rf.kind == "json_schema" && !schemaMatches(v, rf.schema, rf.schema, 0) {
		return "", upstreamFailure("STRUCTURED_OUTPUT_SCHEMA_MISMATCH", 502, "模型输出不符合 response_format 声明的 JSON Schema")
	}
	return candidate, nil
}
