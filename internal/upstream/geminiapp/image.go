package geminiapp

// 用户图片：内联 data: URL 的解码与结构校验，以及原生可续传上传。
// 原始图片字节不进 protobuf：先上传换取私有 Blob ID，回合请求里只引用该 ID。
// 只接受静态 PNG / JPEG / WebP；远程 URL 不代为下载（避免网关被提示词诱导去
// 请求内网地址）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

const (
	maxImages          = 256
	maxImageBytes      = 20 << 20
	maxTotalImageBytes = 256 << 20
	maxImagePixels     = 40_000_000
	maxImageSide       = 32768
	uploadConcurrency  = 4
)

var (
	imageDataURLRe = regexp.MustCompile(`(?i)^data:(image/(?:png|jpeg|webp));base64,([A-Za-z0-9+/]*={0,2})$`)
	pngMagic       = []byte{137, 80, 78, 71, 13, 10, 26, 10}
	pngAnimated    = map[string]bool{"acTL": true, "fcTL": true, "fdAT": true}
)

var (
	errImageBytes    = badRequest("INVALID_IMAGE_BYTES", "图片内容与声明的 MIME 类型不符或已损坏")
	errImageAnimated = badRequest("UNSUPPORTED_ANIMATED_IMAGE", "不支持动图（APNG / 动画 WebP）")
)

// decodeImageDataURL 解码并校验一张内联图片。index 是该图在请求里的序号（报错用）。
func decodeImageDataURL(u string, index int) (imageInput, error) {
	if strings.HasPrefix(strings.ToLower(u), "http:") || strings.HasPrefix(strings.ToLower(u), "https:") {
		return imageInput{}, badRequest("UNSUPPORTED_IMAGE_URL", fmt.Sprintf("第 %d 张图片是远程 URL：Gemini App 上游只支持 data: URL 内联图片", index))
	}
	m := imageDataURLRe.FindStringSubmatch(u)
	if m == nil || m[2] == "" || len(m[2])%4 != 0 {
		return imageInput{}, badRequest("INVALID_IMAGE_DATA_URL", fmt.Sprintf("第 %d 张图片不是合法的 data: URL（仅支持 base64 的 PNG / JPEG / WebP）", index))
	}
	data, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil || len(data) == 0 || base64.StdEncoding.EncodeToString(data) != m[2] {
		return imageInput{}, badRequest("INVALID_IMAGE_DATA_URL", fmt.Sprintf("第 %d 张图片的 base64 内容不合法", index))
	}
	if len(data) > maxImageBytes {
		return imageInput{}, badRequest("IMAGE_TOO_LARGE", fmt.Sprintf("第 %d 张图片超过 20 MiB 上限", index))
	}
	mime := strings.ToLower(m[1])
	width, height, err := imageDimensions(data, mime)
	if err != nil {
		return imageInput{}, err
	}
	if width == 0 || height == 0 || width > maxImageSide || height > maxImageSide || int64(width)*int64(height) > maxImagePixels {
		return imageInput{}, badRequest("IMAGE_DIMENSIONS_TOO_LARGE", fmt.Sprintf("第 %d 张图片的尺寸超出上限（单边 32768 px、总计 4000 万像素）", index))
	}
	return imageInput{mime: mime, data: data}, nil
}

// checkImageBudget 整个请求的图片数量与总字节数上限
func checkImageBudget(images []imageInput) error {
	if len(images) > maxImages {
		return badRequest("IMAGE_COUNT_EXCEEDED", fmt.Sprintf("图片数量超过 %d 张上限", maxImages))
	}
	total := 0
	for _, img := range images {
		total += len(img.data)
	}
	if total > maxTotalImageBytes {
		return badRequest("IMAGES_TOO_LARGE", "图片总大小超过 256 MiB 上限")
	}
	return nil
}

// imageDimensions 只做结构检查（不解码像素）：MIME 必须与容器字节一致，返回宽高
func imageDimensions(b []byte, mime string) (int, int, error) {
	be32 := func(off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }
	le16 := func(off int) int { return int(binary.LittleEndian.Uint16(b[off:])) }
	le32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
	switch mime {
	case "image/png":
		if len(b) < 57 || !bytes.Equal(b[:8], pngMagic) || be32(8) != 13 || string(b[12:16]) != "IHDR" {
			return 0, 0, errImageBytes
		}
		offset, ended, chunks, hasData := 8, false, 0, false
		for offset+12 <= len(b) {
			if chunks++; chunks > 2048 {
				return 0, 0, errImageBytes
			}
			length, typ := int(be32(offset)), string(b[offset+4:offset+8])
			if !isAlpha4(typ) || length > len(b)-offset-12 {
				return 0, 0, errImageBytes
			}
			if pngAnimated[typ] {
				return 0, 0, errImageAnimated
			}
			if (typ == "IHDR" && offset != 8) || be32(offset+8+length) != crc32.ChecksumIEEE(b[offset+4:offset+8+length]) {
				return 0, 0, errImageBytes
			}
			if typ == "IDAT" && length > 0 {
				hasData = true
			}
			offset += length + 12
			if typ == "IEND" {
				if length != 0 || offset != len(b) || !hasData {
					return 0, 0, errImageBytes
				}
				ended = true
				break
			}
		}
		if !ended {
			return 0, 0, errImageBytes
		}
		return int(be32(16)), int(be32(20)), nil
	case "image/jpeg":
		if len(b) < 12 || b[0] != 0xff || b[1] != 0xd8 || b[len(b)-2] != 0xff || b[len(b)-1] != 0xd9 {
			return 0, 0, errImageBytes
		}
		offset, hasDim := 2, false
		var width, height int
		for offset+4 <= len(b) {
			if b[offset] != 0xff {
				return 0, 0, errImageBytes
			}
			offset++
			for offset < len(b) && b[offset] == 0xff {
				offset++
			}
			if offset >= len(b) {
				return 0, 0, errImageBytes
			}
			marker := b[offset]
			offset++
			if marker == 0xd9 { // EOI
				break
			}
			if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) { // 无长度的独立标记
				continue
			}
			if offset+2 > len(b) {
				return 0, 0, errImageBytes
			}
			length := int(binary.BigEndian.Uint16(b[offset:]))
			if length < 2 || length > len(b)-offset {
				return 0, 0, errImageBytes
			}
			if marker == 0xda { // SOS：尺寸必须在此前出现
				if !hasDim || length < 6 || offset+length >= len(b)-2 {
					return 0, 0, errImageBytes
				}
				return width, height, nil
			}
			switch marker { // SOF0 ~ SOF15（不含 DHT / DAC / JPG）
			case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
				if hasDim || length < 8 || b[offset+7] == 0 || length != 8+3*int(b[offset+7]) {
					return 0, 0, errImageBytes
				}
				width, height, hasDim = int(binary.BigEndian.Uint16(b[offset+5:])), int(binary.BigEndian.Uint16(b[offset+3:])), true
			}
			offset += length
		}
		return 0, 0, errImageBytes
	case "image/webp":
		if len(b) < 26 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" || int(le32(4))+8 != len(b) {
			return 0, 0, errImageBytes
		}
		offset, chunks := 12, 0
		var canvas []int
		var pixels []int
		for offset+8 <= len(b) {
			if chunks++; chunks > 2048 {
				return 0, 0, errImageBytes
			}
			kind, size, start := string(b[offset:offset+4]), int(le32(offset+4)), offset+8
			if size > len(b)-start || start+size+(size&1) > len(b) {
				return 0, 0, errImageBytes
			}
			switch kind {
			case "ANIM", "ANMF":
				return 0, 0, errImageAnimated
			case "VP8X":
				if offset != 12 || size != 10 {
					return 0, 0, errImageBytes
				}
				if b[start]&2 != 0 { // 动画标志位
					return 0, 0, errImageAnimated
				}
				canvas = []int{int(b[start+4]) + int(b[start+5])<<8 + int(b[start+6])<<16 + 1, int(b[start+7]) + int(b[start+8])<<8 + int(b[start+9])<<16 + 1}
			case "VP8 ", "VP8L":
				if pixels != nil || (canvas == nil && offset != 12) {
					return 0, 0, errImageBytes
				}
				switch {
				case kind == "VP8 " && size >= 10 && b[start]&1 == 0 && b[start+3] == 0x9d && b[start+4] == 0x01 && b[start+5] == 0x2a:
					pixels = []int{le16(start+6) & 16383, le16(start+8) & 16383}
				case kind == "VP8L" && size >= 5 && b[start] == 0x2f:
					pixels = []int{1 + int(b[start+1]) + int(b[start+2]&63)<<8, 1 + int(b[start+2]>>6) + int(b[start+3])<<2 + int(b[start+4]&15)<<10}
				default:
					return 0, 0, errImageBytes
				}
			}
			offset = start + size + (size & 1)
		}
		if offset != len(b) || pixels == nil || (canvas != nil && (canvas[0] != pixels[0] || canvas[1] != pixels[1])) {
			return 0, 0, errImageBytes
		}
		return pixels[0], pixels[1], nil
	}
	return 0, 0, badRequest("UNSUPPORTED_IMAGE_FORMAT", "不支持的图片格式（仅支持 PNG / JPEG / WebP）")
}

func isAlpha4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if (s[i] < 'A' || s[i] > 'Z') && (s[i] < 'a' || s[i] > 'z') {
			return false
		}
	}
	return true
}

// ── 原生可续传上传 ──────────────────────────────────────────────────────────

// uploadImages 把内联图片上传到 Gemini，按原顺序返回回合请求里引用的附件
func (a *Adapter) uploadImages(ctx context.Context, cli *http.Client, token string, images []imageInput) ([]attachment, error) {
	if err := checkImageBudget(images); err != nil {
		return nil, err
	}
	out := make([]attachment, len(images))
	if len(images) == 0 {
		return out, nil
	}
	noRedirect := *cli
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		sem    = make(chan struct{}, uploadConcurrency)
		failed error
	)
	for i, img := range images {
		wg.Add(1)
		go func(i int, img imageInput) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			att, err := a.uploadOne(ctx, &noRedirect, token, img)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && failed == nil {
				failed = err
				return
			}
			out[i] = att
		}(i, img)
	}
	wg.Wait()
	if failed != nil {
		return nil, failed
	}
	return out, nil
}

// uploadOne 走一次可续传上传：start 建会话拿上传地址，PUT 字节并 finalize 拿 Blob ID
func (a *Adapter) uploadOne(ctx context.Context, cli *http.Client, token string, img imageInput) (attachment, error) {
	origin := a.uploadsOrigin()
	do := func(method, target string, header map[string]string, body []byte, limit int64) (*http.Response, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		req.Header.Set("User-Agent", gatewayUserAgent)
		resp, err := cli.Do(req)
		if err != nil {
			return nil, nil, &turnError{Code: "NATIVE_IMAGE_UPLOAD_RESET", Status: 502, Message: "连接 Gemini 图片上传服务失败：" + err.Error()}
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
		if err != nil {
			return resp, nil, &turnError{Code: "NATIVE_IMAGE_UPLOAD_RESET", Status: 502, Message: "读取 Gemini 图片上传响应失败"}
		}
		return resp, b, nil
	}
	size := itoa(len(img.data))
	resp, _, err := do(http.MethodPost, origin+"/upload/", map[string]string{
		"Authorization":                "Bearer " + strings.TrimSpace(token),
		"Push-Id":                      "feeds/mcudyrk2a4khkz",
		"X-Tenant-Id":                  "bard-storage",
		"X-Goog-Upload-Protocol":       "resumable",
		"X-Goog-Upload-Command":        "start",
		"X-Goog-Upload-Content-Type":   img.mime,
		"X-Goog-Upload-Content-Length": size,
	}, nil, 8192)
	if err != nil {
		return attachment{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return attachment{}, uploadStatusError(resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("X-Goog-Upload-Status"), "active") {
		return attachment{}, uploadInvalid()
	}
	location, err := boundedLocation(origin, resp.Header.Get("X-Goog-Upload-Url"))
	if err != nil {
		return attachment{}, err
	}
	// PUT 阶段不带 Authorization / Push-Id / X-Tenant-Id：上传地址本身就是凭证
	resp, body, err := do(http.MethodPut, location, map[string]string{
		"X-Goog-Upload-Protocol": "resumable",
		"X-Goog-Upload-Command":  "upload, finalize",
		"X-Goog-Upload-Offset":   "0",
	}, img.data, 4096)
	if err != nil {
		return attachment{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return attachment{}, uploadStatusError(resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("X-Goog-Upload-Status"), "final") {
		return attachment{}, uploadInvalid()
	}
	blobID := string(bytes.TrimSpace(body))
	if blobID == "" || len(blobID) > 4096 || strings.ContainsAny(blobID, " \t\r\n") || hasControl(blobID) {
		return attachment{}, uploadInvalid()
	}
	return attachment{BlobID: blobID, MIME: img.mime}, nil
}

// boundedLocation 校验上传地址：必须与上传服务同源、且落在 /upload/ 路径下
func boundedLocation(origin, value string) (string, error) {
	if len(value) > 8192 || strings.ContainsAny(value, "\x00<>\\") || strings.IndexFunc(value, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return "", uploadInvalid()
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", uploadInvalid()
	}
	base, _ := url.Parse(origin)
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) || u.User != nil || u.Fragment != "" ||
		!strings.HasPrefix(u.Path, "/upload/") {
		return "", uploadInvalid()
	}
	return origin + u.RequestURI(), nil
}

func uploadInvalid() error {
	return &turnError{Code: "NATIVE_IMAGE_UPLOAD_INVALID_RESPONSE", Status: 502, Message: "Gemini 图片上传服务返回了无法识别的响应"}
}

// uploadStatusError 上传被拒：按状态码归类
func uploadStatusError(status int) error {
	code, msg := "NATIVE_IMAGE_UPLOAD_INVALID_RESPONSE", "Gemini 图片上传失败"
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		code, msg = "NATIVE_IMAGE_UPLOAD_FORBIDDEN", "Gemini 图片上传被拒（账号无权上传）"
	case http.StatusTooManyRequests:
		code, msg = "NATIVE_IMAGE_UPLOAD_RATE_LIMITED", "Gemini 图片上传被限流（429）"
	default:
		msg = fmt.Sprintf("Gemini 图片上传失败（HTTP %d）", status)
	}
	httpStatus := status
	if httpStatus < 400 || httpStatus > 599 {
		httpStatus = 502
	}
	return &turnError{Code: code, Status: httpStatus, Message: msg}
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
