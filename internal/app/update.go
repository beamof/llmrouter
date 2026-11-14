package app

// ─────────────── 版本检测与自动升级 ───────────────
//
// 检测：读取 GitHub Release 的最新发布，与当前二进制内置的版本号比对。
// 升级：下载当前平台对应的发布资产，用 Release 附带的 SHA256SUMS.txt 校验，
// 替换正在运行的二进制，并按原命令行参数重启。
//
// 替换策略按平台区分（prepareUpdateRestart / finishUpdateRestart）：
//
//   - Unix：运行中的可执行文件允许被改名，直接在本进程内轮换文件，然后用
//     exec 换成新版本，同一 PID 继续运行。systemd（Type=simple）视角服务从未
//     停止；绝不能退出主进程再让子进程接力——主进程一退出，整个 cgroup 里的
//     剩余进程会被 systemd 清理，升级直接中断且服务不会重启。
//   - Windows：运行中的 exe 无法改名或覆盖，先把当前二进制复制一份到临时目录，
//     用副本当「升级助手」：等主进程退出后轮换文件、启动新版本。助手是旧版本
//     的代码，即使新版本有问题也能完成替换。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// appVersion 由发布流水线注入（go build -ldflags "-X main.appVersion=v1.2.3"）；
// 源码直接构建时保持 dev，界面会提示当前为源码构建。
var appVersion = "dev"

const (
	githubRepo        = "beamof/llmrouter"
	githubReleaseAPI  = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	githubReleasesURL = "https://github.com/" + githubRepo + "/releases"

	// updateHelperArg 升级助手的隐藏入口参数，在 main 里先于 flag.Parse 拦截
	updateHelperArg = "-apply-update"
	// updateCheckTTL 检测结果缓存时长，避免反复点按钮撞上 GitHub 未认证限流
	updateCheckTTL = 5 * time.Minute
	// maxUpdateSize 安装包体积上限，防止异常响应把磁盘写满
	maxUpdateSize = 200 << 20
	// updateSwapTimeout 助手等待主进程退出 / 旧文件解锁的最长时间
	updateSwapTimeout = 60 * time.Second
)

// ghAsset GitHub Release 中的一个资产
type ghAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

// ghRelease GitHub Release 响应中用到的字段
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Body        string    `json:"body"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

// updateInfo 一次检测的结果，也是 /api/update/check 的响应体
type updateInfo struct {
	Current      string    `json:"current"`       // 当前版本（dev = 源码构建）
	Latest       string    `json:"latest"`        // 最新发布版本（去掉 v 前缀）
	HasUpdate    bool      `json:"has_update"`    // 是否有可安装的新版本
	OS           string    `json:"os"`            // 运行时平台
	Arch         string    `json:"arch"`          //
	Asset        string    `json:"asset"`         // 与当前平台匹配的发布资产名
	AssetSize    int64     `json:"asset_size"`    // 资产字节数
	ReleaseURL   string    `json:"release_url"`   // 发布页地址
	ReleaseNotes string    `json:"release_notes"` // 发布说明（已截断）
	PublishedAt  time.Time `json:"published_at"`  //
	Notes        string    `json:"notes"`         // 额外提示（平台不支持、源码构建等）
	CheckedAt    time.Time `json:"checked_at"`    //

	assetURL string // 下载地址，仅升级时内部使用
	sumsURL  string // SHA256SUMS.txt 下载地址
}

// versionInfo /api/version 的响应体：只报告本地信息，不访问网络
type versionInfo struct {
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Asset       string `json:"asset"`
	Installable bool   `json:"installable"` // 当前平台是否有预编译发布包可自动升级
}

// updateAssetName 发布资产按 GOOS/GOARCH 命名（见 .github/workflows/release.yml）。
func updateAssetName(goos, goarch string) (string, bool) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[goarch]
	if arch == "" {
		return "", false
	}
	switch goos {
	case "linux":
		return "llmrouter-linux-" + arch, true
	case "windows":
		return "llmrouter-windows-" + arch + ".exe", true
	}
	return "", false
}

// parseVersion 把 v1.2.3 / 1.2.3-rc1 / 1.2.3+meta 解析成数字段；预发布后缀忽略。
func parseVersion(v string) ([]int, bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, false
	}
	out := make([]int, 0, 3)
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// compareVersions 返回 -1/0/1；任一方无法解析时 ok = false。
func compareVersions(a, b string) (int, bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseChecksums 解析 sha256sum 输出：<64 位十六进制>  <文件名>
func parseChecksums(body string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) != 64 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		out[name] = strings.ToLower(fields[0])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SHA256SUMS.txt 内容为空或格式不正确")
	}
	return out, nil
}

func (s *Server) updateClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return http.DefaultClient
}

func (s *Server) ghReleaseURL() string {
	if s.ghAPI != "" {
		return strings.TrimRight(s.ghAPI, "/") + "/releases/latest"
	}
	return githubReleaseAPI
}

// fetchLatestRelease 拉取最新发布信息
func (s *Server) fetchLatestRelease(ctx context.Context) (*ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ghReleaseURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "llmrouter/"+appVersion) // GitHub API 要求 UA
	resp, err := s.updateClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub API 限流（HTTP %d），请稍后再试", resp.StatusCode)
		}
		return nil, fmt.Errorf("GitHub 返回 HTTP %d：%s", resp.StatusCode, errSnippet(body))
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %v", err)
	}
	return &rel, nil
}

// checkUpdate 比对版本并挑出当前平台的发布资产。网络错误才返回 error；
// 平台无发布包等业务性情况写进 Notes，便于界面照常展示当前版本。
func (s *Server) checkUpdate(ctx context.Context) (*updateInfo, error) {
	info := &updateInfo{
		Current:   appVersion,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		CheckedAt: time.Now(),
	}
	rel, err := s.fetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" {
		return nil, fmt.Errorf("GitHub 返回的版本号为空")
	}
	info.Latest = strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	info.ReleaseNotes = truncateRunes(rel.Body, 4000)
	info.PublishedAt = rel.PublishedAt
	info.ReleaseURL = githubReleasesURL
	if rel.HTMLURL != "" {
		info.ReleaseURL = rel.HTMLURL
	}

	switch cmp, ok := compareVersions(appVersion, tag); {
	case !ok:
		// 源码构建（dev）无法比较，提示但仍允许安装发布版
		info.HasUpdate = true
		info.Notes = fmt.Sprintf("当前为源码构建（%s），无法比较版本；升级会把可执行文件替换为发布版。", appVersion)
	case cmp < 0:
		info.HasUpdate = true
	}

	name, ok := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		info.HasUpdate = false
		info.Notes = fmt.Sprintf("当前平台 %s/%s 没有预编译发布包，请到 Release 页面手动下载。", runtime.GOOS, runtime.GOARCH)
		return info, nil
	}
	info.Asset = name
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			info.assetURL, info.AssetSize = a.URL, a.Size
		case "SHA256SUMS.txt":
			info.sumsURL = a.URL
		}
	}
	if info.HasUpdate && info.assetURL == "" {
		info.HasUpdate = false
		info.Notes = fmt.Sprintf("最新发布 %s 中没有 %s，请到 Release 页面手动下载。", info.Latest, name)
	}
	return info, nil
}

// handleVersion 只报告本地版本信息，供设置页立刻显示版本号（不发网络请求）
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	v := versionInfo{Version: appVersion, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if name, ok := updateAssetName(runtime.GOOS, runtime.GOARCH); ok {
		v.Asset, v.Installable = name, true
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	cached := func() *updateInfo {
		s.updMu.Lock()
		defer s.updMu.Unlock()
		if s.upd != nil && time.Since(s.updAt) < updateCheckTTL {
			return s.upd
		}
		return nil
	}
	if r.URL.Query().Get("force") == "" {
		if info := cached(); info != nil {
			writeJSON(w, http.StatusOK, info)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	info, err := s.checkUpdate(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "检测更新失败: "+err.Error())
		return
	}
	s.updMu.Lock()
	s.upd, s.updAt = info, time.Now()
	s.updMu.Unlock()
	writeJSON(w, http.StatusOK, info)
}

// handleUpdateApply 下载并安装最新版本，然后退出等待升级助手重启服务。
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	s.updMu.Lock()
	if s.updRunning {
		s.updMu.Unlock()
		writeErr(w, http.StatusConflict, "已有升级任务正在执行")
		return
	}
	s.updRunning = true
	s.updMu.Unlock()
	release := func() {
		s.updMu.Lock()
		s.updRunning = false
		s.updMu.Unlock()
	}
	fail := func(status int, msg string) {
		release()
		s.logFail(LogEntry{Level: "error", Kind: kindUpdate, Status: status, Message: msg})
		writeErr(w, status, msg)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	info, err := s.checkUpdate(ctx) // 升级不复用缓存：必须是此刻的最新版
	if err != nil {
		fail(http.StatusBadGateway, "检测更新失败: "+err.Error())
		return
	}
	if !info.HasUpdate {
		fail(http.StatusBadRequest, "当前已是最新版本（"+info.Current+"）")
		return
	}
	if info.assetURL == "" {
		fail(http.StatusBadRequest, "最新发布中没有当前平台（"+runtime.GOOS+"/"+runtime.GOARCH+"）的可执行文件")
		return
	}
	if info.sumsURL == "" {
		fail(http.StatusBadRequest, "最新发布缺少 SHA256SUMS.txt，无法校验安装包，已中止")
		return
	}

	newPath, err := s.downloadUpdate(ctx, info)
	if err != nil {
		fail(http.StatusBadGateway, "下载安装包失败: "+err.Error())
		return
	}
	exe, err := executablePath()
	if err != nil {
		os.Remove(newPath)
		fail(http.StatusInternalServerError, "定位当前可执行文件失败: "+err.Error())
		return
	}
	s.store.Flush() // 升级会替换并退出进程，先把热数据写盘
	if err := prepareUpdateRestart(newPath, exe); err != nil {
		os.Remove(newPath)
		fail(http.StatusInternalServerError, "准备升级失败: "+err.Error())
		return
	}

	s.logFail(LogEntry{
		Level: "info", Kind: kindUpdate, Status: http.StatusOK,
		Message: "开始安装新版本 " + info.Latest,
		Detail:  fmt.Sprintf("资产 %s（%d 字节），安装后自动重启", info.Asset, info.AssetSize),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"latest":  info.Latest,
		"message": "已下载并校验 v" + info.Latest + "，服务即将重启",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush() // 先把响应发出去，再退出进程
	}
	time.AfterFunc(700*time.Millisecond, func() { finishUpdateRestart(newPath, exe) })
}

// executablePath 返回当前可执行文件的真实绝对路径（跟随符号链接，避免替换到链接本身）。
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// copySelfToTemp 把当前二进制复制到临时目录，作为升级助手使用：
// 助手不能是正在被替换的那个文件，否则 Windows 下无法改名旧程序。
func copySelfToTemp() (string, error) {
	exe, err := executablePath()
	if err != nil {
		return "", err
	}
	src, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer src.Close()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	helper := filepath.Join(os.TempDir(), fmt.Sprintf("llmrouter-updater-%d-%d%s", os.Getpid(), time.Now().UnixNano(), ext))
	f, err := os.OpenFile(helper, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		os.Remove(helper)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(helper)
		return "", err
	}
	return helper, nil
}

// downloadUpdate 下载安装包到程序目录并校验 SHA256，返回临时文件路径。
// 放在程序目录是为了替换时能原子改名（跨分区 rename 会失败）。
func (s *Server) downloadUpdate(ctx context.Context, info *updateInfo) (string, error) {
	exe, err := executablePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)

	sums, err := s.fetchChecksums(ctx, info.sumsURL)
	if err != nil {
		return "", err
	}
	want, ok := sums[info.Asset]
	if !ok {
		return "", fmt.Errorf("SHA256SUMS.txt 中没有 %s 的校验值", info.Asset)
	}

	f, err := os.CreateTemp(dir, ".llmrouter-update-*") // 能创建即说明目录可写
	if err != nil {
		return "", fmt.Errorf("在程序目录创建临时文件失败（是否缺少写权限？）: %v", err)
	}
	tmp := f.Name()
	f.Close()

	body, err := s.fetchBytes(ctx, info.assetURL)
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		os.Remove(tmp)
		return "", fmt.Errorf("SHA256 校验不通过（期望 %s，实际 %s），已中止安装", want, got)
	}
	if err := os.WriteFile(tmp, body, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// CreateTemp 建的是 0600，WriteFile 不会改已有文件权限；Linux 下缺执行位就无法启动
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func (s *Server) fetchChecksums(ctx context.Context, url string) (map[string]string, error) {
	body, err := s.fetchBytes(ctx, url)
	if err != nil {
		return nil, err
	}
	return parseChecksums(string(body))
}

// fetchBytes 下载一个小文件（安装包 / 校验和），带体积上限。
func (s *Server) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "llmrouter/"+appVersion)
	resp, err := s.updateClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("下载 %s 失败：HTTP %d %s", filepath.Base(url), resp.StatusCode, errSnippet(b))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpdateSize+1))
	if err != nil {
		return nil, fmt.Errorf("下载中断: %v", err)
	}
	if len(body) > maxUpdateSize {
		return nil, fmt.Errorf("文件超过 %d MB，已中止", maxUpdateSize>>20)
	}
	return body, nil
}

// runUpdateHelper 升级助手主循环：等主进程退出 → 轮换文件 → 按原参数重启。
func runUpdateHelper(args []string) {
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[llmrouter-update] "+format+"\n", a...)
	}
	if len(args) < 3 {
		logf("参数不足：需要 <pid> <新文件> <目标路径> [原启动参数...]")
		os.Exit(2)
	}
	pid, _ := strconv.Atoi(args[0])
	newPath, target := args[1], args[2]
	rest := args[3:]

	// start 按原参数拉起 target（新版本或回滚后的旧版本），日志沿用原进程的控制台
	start := func() error {
		cmd := exec.Command(target, rest...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		detachCommand(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		logf("已启动 %s（pid %d）", target, cmd.Process.Pid)
		return nil
	}

	// 等主进程退出，否则新进程会因端口仍被占用而启动失败
	deadline := time.Now().Add(updateSwapTimeout)
	for pid > 0 && processAlive(pid) {
		if time.Now().After(deadline) {
			logf("等待旧进程 %d 退出超时，取消本次升级（旧程序未改动）", pid)
			os.Remove(newPath)
			os.Exit(1)
		}
		time.Sleep(150 * time.Millisecond)
	}
	logf("开始替换 %s", target)
	deadline = time.Now().Add(updateSwapTimeout) // 换文件阶段重新计时

	oldPath := target + ".old"
	os.Remove(oldPath) // 上一次升级残留，先清掉
	var renameErr error
	for {
		if renameErr = os.Rename(target, oldPath); renameErr == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond) // Windows 下旧进程句柄释放稍有延迟
	}
	if renameErr != nil {
		logf("无法备份旧程序: %v，取消本次升级（旧程序仍在原处）", renameErr)
		os.Remove(newPath)
		os.Exit(1)
	}
	if err := os.Rename(newPath, target); err != nil {
		logf("无法写入新程序: %v，已回滚并重启旧版本", err)
		os.Rename(oldPath, target)
		os.Remove(newPath)
		start()
		os.Exit(1)
	}

	if err := start(); err != nil {
		logf("启动新版本失败: %v，已回滚并重启旧版本", err)
		os.Rename(target, newPath)
		os.Rename(oldPath, target)
		start()
		os.Exit(1)
	}
	os.Remove(oldPath)
	os.Exit(0)
}

// cleanupUpdateLeftovers 启动时清理上次升级的残留（旧二进制、临时安装包、过期助手副本）。
func cleanupUpdateLeftovers() {
	if exe, err := executablePath(); err == nil {
		os.Remove(exe + ".old")
		if matches, err := filepath.Glob(filepath.Join(filepath.Dir(exe), ".llmrouter-update-*")); err == nil {
			for _, m := range matches {
				os.Remove(m)
			}
		}
	}
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "llmrouter-updater-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && time.Since(fi.ModTime()) > time.Hour {
			os.Remove(m)
		}
	}
}
