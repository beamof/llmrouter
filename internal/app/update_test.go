package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// withAppVersion 临时改写版本号（模拟发布版/源码构建），测试结束后恢复
func withAppVersion(t *testing.T, v string) {
	t.Helper()
	old := appVersion
	appVersion = v
	t.Cleanup(func() { appVersion = old })
}

func TestUpdateAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
		ok                 bool
	}{
		{"windows", "amd64", "llmrouter-windows-x86_64.exe", true},
		{"windows", "arm64", "llmrouter-windows-arm64.exe", true},
		{"linux", "amd64", "llmrouter-linux-x86_64", true},
		{"linux", "arm64", "llmrouter-linux-arm64", true},
		{"darwin", "arm64", "", false}, // macOS 不在发布矩阵里
		{"linux", "386", "", false},
	}
	for _, c := range cases {
		got, ok := updateAssetName(c.goos, c.goarch)
		if got != c.want || ok != c.ok {
			t.Errorf("updateAssetName(%q,%q) = %q,%v，期望 %q,%v", c.goos, c.goarch, got, ok, c.want, c.ok)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.17.6", "v0.18.0", -1, true},
		{"0.18.0", "v0.18.0", 0, true},
		{"v0.18.1", "v0.18.0", 1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.17.6-rc1", "v0.17.6", 0, true}, // 预发布后缀忽略
		{"1.2", "1.2.0", 0, true},
		{"dev", "v0.18.0", 0, false},
		{"v0.18.0", "latest", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compareVersions(%q,%q) = %d,%v，期望 %d,%v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	body := strings.Join([]string{
		sum + "  llmrouter-linux-x86_64",
		strings.Repeat("cd", 32) + " *llmrouter-windows-x86_64.exe",
		"",
		"not a checksum line",
	}, "\n")
	got, err := parseChecksums(body)
	if err != nil {
		t.Fatal(err)
	}
	if got["llmrouter-linux-x86_64"] != sum {
		t.Errorf("linux 校验值 = %q", got["llmrouter-linux-x86_64"])
	}
	if got["llmrouter-windows-x86_64.exe"] != strings.Repeat("cd", 32) {
		t.Errorf("windows 校验值未去掉 * 前缀: %q", got["llmrouter-windows-x86_64.exe"])
	}
	if _, err := parseChecksums(""); err == nil {
		t.Error("空内容应当报错")
	}
}

func TestCheckUpdate(t *testing.T) {
	asset, ok := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台没有对应的发布资产")
	}
	cases := []struct {
		name       string
		current    string
		tag        string
		wantUpdate bool
		wantNote   string
	}{
		{"发现新版本", "v0.17.6", "v0.18.0", true, ""},
		{"已是最新", "v0.18.0", "v0.18.0", false, ""},
		{"跨大版本", "0.9.9", "v1.0.0", true, ""},
		{"dev 源码构建", "dev", "v0.18.0", true, "源码构建"},
		{"本地版本更高不提示降级", "v0.19.0", "v0.18.0", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withAppVersion(t, c.current)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") == "" || r.Header.Get("User-Agent") == "" {
					t.Errorf("缺少 GitHub 要求的请求头: Accept=%q UA=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
				}
				json.NewEncoder(w).Encode(ghRelease{
					TagName: c.tag,
					HTMLURL: "https://github.com/beamof/llmrouter/releases/tag/" + c.tag,
					Body:    "release notes",
					Assets: []ghAsset{
						{Name: asset, Size: 1234, URL: "https://example.com/bin"},
						{Name: "SHA256SUMS.txt", Size: 400, URL: "https://example.com/sums"},
					},
				})
			}))
			defer srv.Close()

			s := &Server{client: &http.Client{}, ghAPI: srv.URL}
			info, err := s.checkUpdate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.HasUpdate != c.wantUpdate {
				t.Errorf("HasUpdate = %v，期望 %v（notes: %s）", info.HasUpdate, c.wantUpdate, info.Notes)
			}
			if want := strings.TrimPrefix(c.tag, "v"); info.Latest != want {
				t.Errorf("Latest = %q，期望 %q", info.Latest, want)
			}
			if info.Asset != asset || info.AssetSize != 1234 {
				t.Errorf("资产信息 = %q/%d，期望 %q/1234", info.Asset, info.AssetSize, asset)
			}
			if c.wantUpdate {
				if info.assetURL == "" || info.sumsURL == "" {
					t.Error("有更新时应同时给出资产与校验和地址")
				}
			}
			if c.wantNote != "" && !strings.Contains(info.Notes, c.wantNote) {
				t.Errorf("Notes = %q，期望包含 %q", info.Notes, c.wantNote)
			}
		})
	}
}

func TestCheckUpdateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()
	s := &Server{client: &http.Client{}, ghAPI: srv.URL}
	if _, err := s.checkUpdate(context.Background()); err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("403 应报限流错误，得到 %v", err)
	}
}

func TestDownloadUpdateVerifiesChecksum(t *testing.T) {
	asset, ok := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台没有对应的发布资产")
	}
	bin := []byte("fake-release-binary")
	sum := sha256.Sum256(bin)

	var sumsBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sumsBody) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := &Server{client: &http.Client{}}
	sumsBody = fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)
	path, err := s.downloadUpdate(context.Background(), &updateInfo{
		Asset: asset, assetURL: srv.URL + "/bin", sumsURL: srv.URL + "/sums",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bin) {
		t.Errorf("下载内容 = %q，期望 %q", got, bin)
	}
	if filepath.Dir(path) == "" {
		t.Error("临时文件应落在程序目录")
	}
	dir := filepath.Dir(path)
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("下载的二进制缺少执行位: %v", fi.Mode())
		}
	}
	os.Remove(path) // 先清掉成功产物，下面才能确认失败路径不留残留

	// 校验值不匹配时必须中止且不留下文件
	sumsBody = fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), asset)
	if _, err := s.downloadUpdate(context.Background(), &updateInfo{
		Asset: asset, assetURL: srv.URL + "/bin", sumsURL: srv.URL + "/sums",
	}, nil); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("校验不通过应报错，得到 %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".llmrouter-update-*")); len(left) > 0 {
		for _, p := range left {
			os.Remove(p)
		}
		t.Errorf("失败的下载不应留下临时文件: %v", left)
	}

	// 校验和文件里缺少当前平台资产
	sumsBody = fmt.Sprintf("%s  other-binary\n", strings.Repeat("0", 64))
	if _, err := s.downloadUpdate(context.Background(), &updateInfo{
		Asset: asset, assetURL: srv.URL + "/bin", sumsURL: srv.URL + "/sums",
	}, nil); err == nil || !strings.Contains(err.Error(), "没有") {
		t.Fatalf("缺少资产校验值应报错，得到 %v", err)
	}
}

// TestUpdateApplyAsyncTask 升级是后台任务：POST 立即返回，状态经
// /api/update/status 轮询；任务独立于发起请求的连接（客户端断开不中止下载）。
func TestUpdateApplyAsyncTask(t *testing.T) {
	asset, ok := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台没有对应的发布资产")
	}
	withAppVersion(t, "v1.0.0")
	bin := bytes.Repeat([]byte("n100-slow-network-payload"), 64*1024) // ~1.6MB
	sum := sha256.Sum256(bin)

	var gh *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ghRelease{
			TagName: "v2.0.0",
			Assets: []ghAsset{
				{Name: asset, Size: int64(len(bin)), URL: gh.URL + "/slow/bin"},
				{Name: "SHA256SUMS.txt", Size: 400, URL: gh.URL + "/slow/sums"},
			},
		})
	})
	mux.HandleFunc("/slow/bin", func(w http.ResponseWriter, r *http.Request) {
		// 分块慢速输出，模拟 N100 直连 GitHub 的慢网络
		w.Header().Set("Content-Length", fmt.Sprint(len(bin)))
		for off := 0; off < len(bin); off += 32 * 1024 {
			end := off + 32*1024
			if end > len(bin) {
				end = len(bin)
			}
			w.Write(bin[off:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	mux.HandleFunc("/slow/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
	})
	gh = httptest.NewServer(mux)
	defer gh.Close()

	st, err := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var installedPath string
	installed := make(chan string, 1)
	s := &Server{store: st, client: &http.Client{}, ghAPI: gh.URL,
		installUpdate: func(newPath, exe string) error {
			installedPath = newPath
			installed <- newPath
			return nil
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/update/apply", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	s.handleUpdateApply(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply 应立即返回 200: %d %s", rec.Code, rec.Body.String())
	}
	var started struct {
		OK      bool   `json:"ok"`
		Latest  string `json:"latest"`
		Message string `json:"message"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	if !started.OK || started.Latest != "2.0.0" {
		t.Fatalf("启动响应: %+v", started)
	}
	cancel() // 客户端立刻断开：任务必须照常完成

	// 轮询任务状态直到安装钩子被调用
	select {
	case p := <-installed:
		if p == "" || installedPath != p {
			t.Fatalf("安装路径异常: %q", p)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("后台任务未在预期时间内完成下载与校验")
	}
	// 终态：done，且「开始安装」日志已入 store（exec 前落盘的修复点）
	deadline := time.Now().Add(5 * time.Second)
	for {
		srec := httptest.NewRecorder()
		s.handleUpdateStatus(srec, httptest.NewRequest(http.MethodGet, "/api/update/status", nil))
		var status struct {
			Running bool        `json:"running"`
			Task    *updateTask `json:"task"`
		}
		json.Unmarshal(srec.Body.Bytes(), &status)
		if status.Task != nil && status.Task.Stage == "done" {
			if status.Running {
				t.Fatal("done 后任务不应仍在执行")
			}
			if status.Task.Received != int64(len(bin)) || status.Task.Total != int64(len(bin)) {
				t.Fatalf("下载进度: %+v", status.Task)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务未到 done: %+v", status.Task)
		}
		time.Sleep(20 * time.Millisecond)
	}
	foundLog := false
	for _, l := range st.Logs {
		if strings.Contains(l.Message, "开始安装新版本") {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatal("「开始安装」日志应在 exec 前写入 store")
	}
	os.Remove(installedPath)
}

// TestUpdateApplyAsyncFailure 校验失败：任务转 failed、原因可见、服务不受影响
func TestUpdateApplyAsyncFailure(t *testing.T) {
	asset, ok := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("当前平台没有对应的发布资产")
	}
	withAppVersion(t, "v1.0.0")
	var gh *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ghRelease{TagName: "v2.0.0", Assets: []ghAsset{
			{Name: asset, Size: 10, URL: gh.URL + "/bin"},
			{Name: "SHA256SUMS.txt", Size: 400, URL: gh.URL + "/sums"},
		}})
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("tampered!!")) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), asset)
	})
	gh = httptest.NewServer(mux)
	defer gh.Close()

	st, _ := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	s := &Server{store: st, client: &http.Client{}, ghAPI: gh.URL,
		installUpdate: func(string, string) error { t.Fatal("校验失败不应走到安装"); return nil },
	}
	rec := httptest.NewRecorder()
	s.handleUpdateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("apply 应启动任务: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		srec := httptest.NewRecorder()
		s.handleUpdateStatus(srec, httptest.NewRequest(http.MethodGet, "/api/update/status", nil))
		var status struct {
			Running bool        `json:"running"`
			Task    *updateTask `json:"task"`
		}
		json.Unmarshal(srec.Body.Bytes(), &status)
		if status.Task != nil && status.Task.Stage == "failed" {
			if !strings.Contains(status.Task.Message, "下载安装包失败") {
				t.Fatalf("失败原因: %+v", status.Task)
			}
			if status.Running {
				t.Fatal("失败后任务不应仍在执行")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务未到 failed: %+v", status.Task)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUpdateApplyConflict 任务执行中重复 apply 应被拒绝
func TestUpdateApplyConflict(t *testing.T) {
	withAppVersion(t, "v1.0.0")
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			json.NewEncoder(w).Encode(ghRelease{TagName: "v2.0.0"})
			return
		}
		<-block // 卡住资产下载
	}))
	defer srv.Close()
	defer close(block)
	st, _ := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	s := &Server{store: st, client: &http.Client{}, ghAPI: srv.URL}
	// 手工把任务置为执行中，验证并发拒绝
	s.updMu.Lock()
	s.updRunning = true
	s.updMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleUpdateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("执行中重复 apply 应 409: %d", rec.Code)
	}
}

func TestHandleVersion(t *testing.T) {
	withAppVersion(t, "v9.9.9")
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleVersion(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var v versionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != "v9.9.9" || v.OS != runtime.GOOS || v.Arch != runtime.GOARCH {
		t.Errorf("版本信息 = %+v", v)
	}
}

func TestUpdateApplyNoUpdate(t *testing.T) {
	withAppVersion(t, "v1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ghRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	st, err := LoadStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, client: &http.Client{}, ghAPI: srv.URL}
	rec := httptest.NewRecorder()
	s.handleUpdateApply(rec, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rec.Code, rec.Body.String())
	}
	if s.updRunning {
		t.Error("失败的升级不应把任务标记为执行中")
	}
}

func TestUpdateCheckCache(t *testing.T) {
	withAppVersion(t, "v1.0.0")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		json.NewEncoder(w).Encode(ghRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()
	s := &Server{client: &http.Client{}, ghAPI: srv.URL}

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleUpdateCheck(rec, httptest.NewRequest(http.MethodGet, "/api/update/check", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
		}
	}
	if hits != 1 {
		t.Errorf("短时间内的第二次检测应命中缓存，实际请求 %d 次", hits)
	}

	// force=1 绕过缓存
	rec := httptest.NewRecorder()
	s.handleUpdateCheck(rec, httptest.NewRequest(http.MethodGet, "/api/update/check?force=1", nil))
	if hits != 2 {
		t.Errorf("force=1 应重新请求，实际 %d 次", hits)
	}
}
