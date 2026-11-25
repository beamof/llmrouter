package geminiapp

import (
	"regexp"
	"strings"
)

// modelSpec 一个对外模型的元数据
type modelSpec struct {
	Mode          int            // 请求的原生 ModeCategory（出站字段 47）
	ContextLength int64          // 向客户端声明的上下文窗口（tokens）；0 = 不声明
	Attribution   *regexp.Regexp // 上游模型归属（响应字段 30）必须匹配；nil = 不校验
}

// nativeModels 对外模型，顺序即后台目录顺序。原生 ModeCategory：PRO = 3
// （Gemini 4 Neon）、FAST = 1（Gemini 4 Hydrogen）、FLASH_LITE = 6。
var nativeModels = []string{"gemini-neon", "gemini-hydrogen", "gemini-flash-lite"}

var modelSpecs = map[string]modelSpec{
	"gemini-neon":       {Mode: 3, ContextLength: 512000, Attribution: regexp.MustCompile(`(?i)\bneon\b`)},
	"gemini-hydrogen":   {Mode: 1, ContextLength: 512000, Attribution: regexp.MustCompile(`(?i)\bhydrogen\b`)},
	"gemini-flash-lite": {Mode: 6},
}

// modelAliases 过渡期的兼容别名：上游已下线的旧名指到新模型
var modelAliases = map[string]string{"gemini-argon": "gemini-neon"}

// canonicalModel 把请求里的模型名归一到对外模型；不认识时返回 false。
func canonicalModel(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "models/") {
		name = name[len("models/"):]
	}
	if alias, ok := modelAliases[name]; ok {
		name = alias
	}
	if _, ok := modelSpecs[name]; !ok {
		return "", false
	}
	return name, true
}

// effortLevels OpenAI 思考等级 → Gemini 同步思考档位。档位是协议枚举而不是线性
// 刻度：2（扩展思考）是普通回合的默认值，3 是异步 Deep Think（本网关不支持）。
var effortLevels = map[string]int{
	"none": 0, "minimal": 0, "low": 1, "medium": 2, "high": 2,
	"xhigh": 2, "max": 2, "ultra": 2,
}

// ── 请求元数据 ──────────────────────────────────────────────────────────────

// turnRequestMetadata ROBINRequestMetadata 的子集：客户端类型、语言、版本与
// 每回合临时的实例标识，不含设备 ID 或硬件证明。
func turnRequestMetadata(instanceID string) []byte {
	var b []byte
	b = append(b, pbBytes(1, pbVarint(1, 1))...)             // 1: 触发类型
	b = append(b, pbBytes(2, pbString(1, "en"))...)          // 2: 界面语言
	b = append(b, pbBytes(3, pbString(1, nativeVersion))...) // 3: 客户端版本
	b = append(b, pbVarint(4, nativeClientType)...)          // 4: 客户端类型
	b = append(b, pbString(7, "en")...)                      // 7: 语言
	b = append(b, pbVarint(5, 1)...)                         // 5: UI 来源
	b = append(b, pbString(23, instanceID)...)               // 23: 客户端实例标识
	return b
}

// termsRequestMetadata 条款 RPC 的元数据：与生成请求相同的客户端类型与界面来源
func termsRequestMetadata(language string) []byte {
	var b []byte
	b = append(b, pbBytes(2, pbString(1, language))...)
	b = append(b, pbVarint(4, nativeClientType)...)
	b = append(b, pbString(7, language)...)
	b = append(b, pbVarint(5, 1)...)
	return b
}

// ── 出站：ProcessQuery 请求 ─────────────────────────────────────────────────

// attachment 已上传到 Gemini 的图片附件（出站只带 Blob ID，原始字节走上传通道）
type attachment struct {
	BlobID string
	MIME   string
}

var imageExts = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}

// buildTurnWire 装配 ProcessQuery 的请求体：三条 gRPC 帧依次是初始请求、
// 用户文本和结束标记。整段对话折成单条用户文本（userText），系统指令与工具
// 协议都渲染在里面；临时会话（temporaryChat）让这一轮不进账号的云端历史。
func buildTurnWire(prompt string, images []attachment, mode, thinking int) []byte {
	instanceID := newUUID()
	query := pbString(1, prompt)
	for i, img := range images {
		// Attachment.blob：ID（1）、类型 = 图片（2 = 1）、MIME（3）、来源 = 用户上传（4 = 1）
		blob := concat(pbString(1, img.BlobID), pbVarint(2, 1), pbString(3, img.MIME), pbVarint(4, 1))
		name := pbString(2, "image-"+itoa(i+1)+"."+imageExts[img.MIME])
		query = append(query, pbBytes(3, concat(pbBytes(1, blob), name))...)
	}
	template := concat(
		pbBytes(1, query),
		pbVarint(5, 1), // 5
		pbBytes(13, turnRequestMetadata(instanceID)), // 13: 请求元数据
		pbVarint(15, 1),                  // 15: 未声明原生工具
		pbVarint(31, 1),                  // 31: 临时会话
		pbVarint(55, 0), pbVarint(56, 0), // 55 / 56: 内置工具与智能选模关闭
		pbVarint(47, uint64(mode)),     // 47: 请求的模型档位
		pbVarint(48, uint64(thinking)), // 48: 请求的思考档位
	)
	initial := concat(
		pbBytes(1, turnRequestMetadata(instanceID)),
		pbBytes(1001, pbBytes(1, template)),
		pbBytes(1004, nil), // 1004: 空的扩展标记位
	)
	return concat(
		grpcFrame(concat(pbVarint(1, 1), pbBytes(1000, initial))),
		grpcFrame(concat(pbVarint(1, 1), pbBytes(1007, pbString(1, prompt)))),
		grpcFrame(concat(pbVarint(1, 1), pbBytes(1007, pbBytes(2, nil)))),
	)
}
