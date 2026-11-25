package geminiapp

// 与参考实现 gemini-app2api 的差分测试。testdata/golden.json 由参考实现的纯编码
// 函数生成：ProcessQuery 请求体字节、tool_call 围栏解析的事件流、提示词文本。
// 这里用同样的输入跑移植结果并逐项比对。

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

type goldenFile struct {
	Wire []struct {
		Name     string `json:"name"`
		Mode     int    `json:"mode"`
		Thinking int    `json:"thinking"`
		Text     string `json:"text"`
		Images   []struct {
			BlobID string `json:"blobId"`
			MIME   string `json:"mimeType"`
		} `json:"images"`
		Hex string `json:"hex"`
	} `json:"wire"`
	Terms struct {
		Version string `json:"version"`
	} `json:"terms"`
	Splitter []struct {
		Name   string        `json:"name"`
		Choice string        `json:"choice"`
		Chunk  int           `json:"chunk"`
		Text   string        `json:"text"`
		Events []goldenEvent `json:"events"`
		Error  string        `json:"error"`
	} `json:"splitter"`
	Prompts []struct {
		Name      string `json:"name"`
		Body      string `json:"body"`
		Responses bool   `json:"responses"`
		Prompt    string `json:"prompt"`
		Error     string `json:"error"`
	} `json:"prompts"`
}

// goldenEvent {text: "..."} 或 {call: [工具名, 实参 JSON]}
type goldenEvent struct {
	Text *string  `json:"text"`
	Call []string `json:"call"`
}

func loadGolden(t *testing.T) *goldenFile {
	t.Helper()
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

// goldenTools 与生成脚本相同的工具声明
var goldenTools = []any{map[string]any{"type": "function", "function": map[string]any{
	"name": "fixture_lookup", "description": "Synthetic lookup",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{"type": "string"},
			"n":   map[string]any{"type": "integer", "minimum": float64(0)},
		},
		"required": []any{"key"}, "additionalProperties": false,
	},
}}}

func goldenChoice(choice string) any {
	switch choice {
	case "none", "required":
		return choice
	case "forced":
		return map[string]any{"type": "function", "function": map[string]any{"name": "fixture_lookup"}}
	}
	return "auto"
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`)

// 每回合的实例标识是随机 UUID：换成与黄金数据相同的等长占位后再比对
func normalizeUUID(b []byte) []byte {
	return uuidRe.ReplaceAll(b, []byte("00000000-0000-4000-8000-000000000000"))
}

func assertHexEqual(t *testing.T, name, gotHex, wantHex string) {
	t.Helper()
	if gotHex == wantHex {
		return
	}
	got, want := []byte(gotHex), []byte(wantHex)
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	t.Errorf("%s 请求体与参考实现不一致：首个差异在第 %d 个十六进制字符（共 got %d / want %d）\n got: …%s\nwant: …%s",
		name, at, len(gotHex), len(wantHex), safeSlice(gotHex, at-40, at+64), safeSlice(wantHex, at-40, at+64))
}

func safeSlice(s string, from, to int) string {
	if from < 0 {
		from = 0
	}
	if to > len(s) {
		to = len(s)
	}
	if from >= to {
		return ""
	}
	return s[from:to]
}

func TestGoldenWire(t *testing.T) {
	g := loadGolden(t)
	for _, w := range g.Wire {
		images := make([]attachment, 0, len(w.Images))
		for _, img := range w.Images {
			images = append(images, attachment{BlobID: img.BlobID, MIME: img.MIME})
		}
		got := normalizeUUID(buildTurnWire(w.Text, images, w.Mode, w.Thinking))
		assertHexEqual(t, w.Name, hex.EncodeToString(got), w.Hex)
	}
	// 条款版本查询的请求体
	assertHexEqual(t, "terms-version",
		hex.EncodeToString(concat(pbString(1, "en"), pbBytes(2, termsRequestMetadata("en")))), g.Terms.Version)
}

// splitterDivergences 与参考实现有意不同的用例及本网关的预期
var splitterDivergences = map[string]string{
	// JS 把超过 2^53 的整数判为「不是安全整数」而拒绝；Go 按原文字保留大整数，照常放行
	"unicode-args": "OK",
}

func TestGoldenSplitter(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.Splitter {
		t.Run(fmt.Sprintf("%s/%d/%s", c.Name, c.Chunk, c.Choice), func(t *testing.T) {
			policy := compileToolPolicy(goldenTools, goldenChoice(c.Choice))
			s := newFenceSplitter(policy)
			var events []goldenEvent
			var code string
			collect := func(items []splitEvent, err error) bool {
				for _, it := range items {
					if it.isCall {
						events = append(events, goldenEvent{Call: []string{it.call.Function.Name, it.call.Function.Arguments}})
					} else {
						text := it.text
						events = append(events, goldenEvent{Text: &text})
					}
				}
				if err != nil && code == "" {
					code = turnErrorCode(err)
				}
				return err != nil
			}
			push := func(part string) bool {
				return collect(s.push(part))
			}
			switch {
			case c.Chunk == -1: // 按行切块
				for len(c.Text) > 0 {
					i := strings.IndexByte(c.Text, '\n')
					if i < 0 {
						i = len(c.Text) - 1
					}
					if push(c.Text[:i+1]) {
						c.Text = ""
						break
					}
					c.Text = c.Text[i+1:]
				}
			case c.Chunk > 0:
				for i := 0; i < len(c.Text); i += c.Chunk {
					end := i + c.Chunk
					if end > len(c.Text) {
						end = len(c.Text)
					}
					if push(c.Text[i:end]) {
						break
					}
				}
			default:
				push(c.Text)
			}
			if code == "" {
				items, err := s.flush()
				collect(items, err)
			}
			if want := splitterDivergences[c.Name]; want != "" {
				got := code
				if got == "" {
					got = "OK"
				}
				if got != want {
					t.Fatalf("错误码 = %s，本网关预期 %s（与参考实现的有意差异）", got, want)
				}
				return
			}
			if code != c.Error {
				t.Errorf("错误码 = %q，参考实现 %q", code, c.Error)
			}
			if !eventsEqual(events, c.Events) {
				t.Errorf("事件流与参考实现不一致\n got: %s\nwant: %s", eventsDebug(events), eventsDebug(c.Events))
			}
		})
	}
}

func turnErrorCode(err error) string {
	te := &turnError{}
	if errors.As(err, &te) {
		return te.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func eventsEqual(a, b []goldenEvent) bool {
	return bytes.Equal([]byte(eventsDebug(a)), []byte(eventsDebug(b)))
}

func eventsDebug(events []goldenEvent) string {
	var sb strings.Builder
	for i, e := range events {
		if i > 0 {
			sb.WriteString(" | ")
		}
		if e.Text != nil {
			fmt.Fprintf(&sb, "text(%d)%q", len(*e.Text), *e.Text)
		} else {
			fmt.Fprintf(&sb, "call%q", e.Call)
		}
	}
	return sb.String()
}

// promptDivergences 参考实现直接拒绝、本网关从宽处理的请求（不做逐字比对）
var promptDivergences = map[string]bool{"chat-annotations": true}

func TestGoldenPrompts(t *testing.T) {
	g := loadGolden(t)
	for _, p := range g.Prompts {
		t.Run(p.Name, func(t *testing.T) {
			plan, err := buildPlan([]byte(p.Body), p.Responses)
			if promptDivergences[p.Name] {
				if err != nil || plan == nil {
					t.Fatalf("参考实现拒绝的请求，本网关应从宽处理成功，got err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// 工具声明 / 输出 schema 是 JSON：参考实现保留客户端的键顺序，本网关按键名
			// 排序。语义等价即可，比对前把这些「整行即 JSON」的行规范化。
			got, want := normalizeJSONLines(plan.prompt), normalizeJSONLines(p.Prompt)
			if got != want {
				at := 0
				for at < len(got) && at < len(want) && got[at] == want[at] {
					at++
				}
				t.Errorf("提示词与参考实现不一致：首个差异在第 %d 个字节（got %d / want %d）\n got: %q\nwant: %q",
					at, len(got), len(want), safeSlice(got, at-80, at+200), safeSlice(want, at-80, at+200))
			}
		})
	}
}

// normalizeJSONLines 把提示词里「整行即 JSON」的行按键名规范化（其余行原样保留）
func normalizeJSONLines(prompt string) string {
	lines := strings.Split(prompt, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, `{`) && !strings.HasPrefix(line, `[`) {
			continue
		}
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		if b, err := json.Marshal(v); err == nil {
			lines[i] = string(b)
		}
	}
	return strings.Join(lines, "\n")
}
