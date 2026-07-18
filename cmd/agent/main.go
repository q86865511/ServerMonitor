// Command agent 是 ServerMonitor 的獨立節點代理執行檔(R5 多節點):在遠端/雲端主機上運行,對外
// 提供與 in-process 代理等價的 HTTP/WS API(bearer token 認證、預設自簽 TLS + SHA-256 指紋 TOFU),
// 由本機 GUI 經 NodeClient 管理。組裝鏈(dispatch/native/docker)與 in-process 完全一致(app.NewDefaultBackend),
// 確保備份 tar 格式、資料目錄結構與行為互通。
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"servermonitor/internal/agent"
	"servermonitor/internal/app"
)

func main() {
	configPath := flag.String("config", "", "設定檔路徑(agent.toml);預設為執行檔旁 agent.toml")
	flag.Parse()

	if err := run(*configPath); err != nil {
		log.Fatalf("agent: %v", err)
	}
}

// run 載入設定、備妥身分(token/憑證)、組裝後端與代理 server,啟動 HTTP(S) 服務並在收到終止訊號
// 時優雅關閉。回傳非 nil 錯誤即致命(main 以非零碼退出)。
func run(configPath string) error {
	cfg, err := loadAgentConfig(configPath)
	if err != nil {
		return err
	}

	// 1) 目錄骨架。
	for _, d := range []string{cfg.DataRoot, cfg.BackupRoot, cfg.CacheRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("建立目錄 %s 失敗: %w", d, err)
		}
	}

	// 2) 持久 token(首啟生成)。
	token, tokenGenerated, err := loadOrCreateToken(cfg.TokenFile)
	if err != nil {
		return err
	}

	// 3) TLS 憑證與 server TLS 設定(auto 首啟生成自簽,custom 載入正式憑證,off 純明文)。
	var (
		tlsCfg      *tls.Config
		fingerprint string
		certNote    string
	)
	switch cfg.TLS.Mode {
	case tlsModeOff:
		certNote = "TLS 已停用(純明文 HTTP)——僅限內網/VPN 使用,請勿暴露於公網"
	case tlsModeAuto, tlsModeCustom:
		cert, fp, generated, cerr := loadOrCreateCert(cfg.TLS.Mode, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if cerr != nil {
			return cerr
		}
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		fingerprint = fp
		if generated {
			certNote = "已生成自簽憑證(" + cfg.TLS.CertFile + ")"
		} else {
			certNote = "已載入既有憑證(" + cfg.TLS.CertFile + ")"
		}
	default:
		return fmt.Errorf("未知的 tls.mode %q(可用 auto/custom/off)", cfg.TLS.Mode)
	}

	// 4) 組裝與 in-process 等價的頂層後端(Windows dispatch(native+docker)、其餘 docker-only)。
	//    Docker 不可用時 dispatch 頂層仍在線(native),沿用既有降級行為。
	backend, err := app.NewDefaultBackend(agent.BackendOptions{
		InstancesRoot: cfg.DataRoot,
		BackupRoot:    cfg.BackupRoot,
		CacheRoot:     cfg.CacheRoot,
		Node:          cfg.Node,
	})
	if err != nil {
		return fmt.Errorf("建立執行後端失敗: %w", err)
	}
	defer closeBackend(backend)

	// 5) 代理 server(注入持久 token,使 GUI 端 token 跨重啟不變)。
	srv, err := agent.NewServer(agent.Config{Backend: backend, Token: token})
	if err != nil {
		return fmt.Errorf("建立節點代理失敗: %w", err)
	}
	defer srv.Close()

	// 逾時:agent 綁 0.0.0.0 公開暴露,bearer 驗證要等標頭讀完才執行,故未設逾時會被
	// Slowloris(緩慢送標頭/長掛 keep-alive)佔滿連線耗盡 fd。ReadHeaderTimeout 擋慢標頭、
	// IdleTimeout 回收閒置 keep-alive;不設 WriteTimeout/ReadTimeout 以免截斷長輪詢日誌串流與大檔上傳。
	httpSrv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           srv,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 6) 啟動訊息:監聽位址、憑證狀態與指紋、token(提示複製到 GUI)。
	printBanner(cfg, fingerprint, certNote, token, tokenGenerated)

	// 7) 服務 + 優雅關閉。
	errCh := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			errCh <- httpSrv.ListenAndServeTLS("", "") // 憑證已在 TLSConfig
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("HTTP 服務結束: %w", err)
		}
		return nil
	case sig := <-sigCh:
		log.Printf("agent: 收到訊號 %v,開始優雅關閉…", sig)
		// 先 Close(cancel server context)主動中止 hijack 的 WS 日誌串流 goroutine,
		// 否則 Shutdown 會枯等作用中串流直到 10s 逾時才強制結束(Close 冪等,defer 再呼叫無害)。
		_ = srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("agent: 關閉逾時/失敗(強制結束): %v", err)
			_ = httpSrv.Close()
		}
		return nil
	}
}

// printBanner 於啟動時把運行資訊印到 stdout。指紋與 token 供使用者複製到 GUI 新增節點對話框。
func printBanner(cfg agentConfig, fingerprint, certNote, token string, tokenGenerated bool) {
	scheme := "https"
	if cfg.TLS.Mode == tlsModeOff {
		scheme = "http"
	}
	fmt.Println("====================================================================")
	fmt.Println(" ServerMonitor 節點代理已啟動")
	fmt.Printf("  節點名稱 : %s\n", cfg.Node)
	fmt.Printf("  監聽位址 : %s (%s)\n", cfg.Bind, scheme)
	fmt.Printf("  資料目錄 : %s\n", cfg.DataRoot)
	fmt.Printf("  TLS      : %s\n", certNote)
	if fingerprint != "" {
		fmt.Printf("  憑證指紋 : %s\n", fingerprint)
		fmt.Println("             (於 GUI 新增節點時比對此指紋以確認信任)")
	}
	tokenNote := "既有 token"
	if tokenGenerated {
		tokenNote = "本次新生成"
	}
	fmt.Printf("  Token    : %s  (%s)\n", token, tokenNote)
	fmt.Println("             (複製此 token 到 GUI 新增節點對話框)")
	fmt.Println("  按 Ctrl+C 停止。")
	fmt.Println("====================================================================")
}

// closeBackend 關閉實作了 Close 的後端(dispatch/docker/native 皆有;RuntimeBackend 介面未含 Close)。
func closeBackend(b agent.RuntimeBackend) {
	if c, ok := b.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}
