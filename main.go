package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llmrouter/internal/app"
)

func main() {
	// 自动升级的助手入口，必须先于 flag.Parse 拦截
	if app.IsUpdateHelperArgs(os.Args) {
		app.RunUpdateHelper(os.Args[2:])
		return
	}

	addr := flag.String("addr", getenv("LLMROUTER_ADDR", "127.0.0.1:8080"), "监听地址")
	data := flag.String("data", getenv("LLMROUTER_DATA", "data.json"), "状态文件路径")
	adminToken := flag.String("admin-token", os.Getenv("LLMROUTER_ADMIN_TOKEN"), "管理后台令牌（空 = 不鉴权）")
	apiToken := flag.String("api-token", os.Getenv("LLMROUTER_API_TOKEN"), "代理入口令牌（空 = 不鉴权）")
	trustProxy := flag.Bool("trust-proxy", os.Getenv("LLMROUTER_TRUST_PROXY") != "", "部署在反向代理后时开启：按 X-Forwarded-For 判定来源 IP（默认取 TCP 对端）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(app.Version())
		return
	}

	app.CleanupUpdateLeftovers() // 清掉上次升级留下的旧二进制与临时文件

	s, err := app.New(app.Config{
		Data:       *data,
		AdminToken: *adminToken,
		APIToken:   *apiToken,
		TrustProxy: *trustProxy,
	})
	if err != nil {
		log.Fatalf("读取 %s 失败: %v", *data, err)
	}
	if *adminToken == "" {
		log.Println("提示：未设置管理令牌，后台完全开放；建议 -admin-token XXX 或环境变量 LLMROUTER_ADMIN_TOKEN")
	}
	if !s.Store().HasAPIKeys() && *apiToken == "" {
		log.Println("提示：未配置任何 API Key，代理入口不鉴权；可在后台 Keys 页生成")
	}
	go s.StartBackground() // WorkBuddy 每日签到 + 余额快照

	srv := &http.Server{Addr: *addr, Handler: s.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// 先停止接收新请求并等待在途请求结束，再落盘，避免 flush 后又有新用量。
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
		s.Shutdown()
	}()

	log.Printf("LLM Router %s 已启动：管理后台 http://%s | 代理入口 /v1/chat/completions /v1/responses /v1/models", app.Version(), *addr)
	log.Fatal(srv.ListenAndServe())
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
