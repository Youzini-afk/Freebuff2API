package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Quorinex/Freebuff2API/webui"
)

func main() {
	configPath := flag.String("config", "", "path to a JSON config file (default: config.json if present)")
	flag.Parse()

	logger := log.New(os.Stdout, "[Freebuff2API] ", log.LstdFlags|log.Lmsgprefix)

	// Auto-detect config.json in CWD when no flag is given
	if *configPath == "" {
		if info, err := os.Stat("config.json"); err == nil && info.Mode().IsRegular() {
			*configPath = "config.json"
		}
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.HTTPProxy != "" {
		importURL, _ := url.Parse(cfg.HTTPProxy)
		transport.Proxy = http.ProxyURL(importURL)
	}
	httpClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	
	registry := NewModelRegistry(httpClient, logger)
	registry.Start(context.Background())
	defer registry.Stop()

	storePath := filepath.Join(cfg.DataDir, "tokens.json")
	store, err := NewTokenStore(storePath, cfg.AuthTokens)
	if err != nil {
		logger.Fatalf("init token store: %v", err)
	}
	logger.Printf("token store: %d managed tokens (file: %s)", len(store.List()), storePath)

	metricsPath := filepath.Join(cfg.DataDir, "metrics.json")
	metrics, err := NewMetrics(metricsPath)
	if err != nil {
		logger.Fatalf("init metrics: %v", err)
	}
	metrics.StartBackgroundFlush()
	defer metrics.Close()

	server := NewServer(cfg, logger, registry, metrics)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	server.runs.Reconcile(runCtx, store.List())
	server.Start(runCtx)

	var admin *AdminHandler
	if strings.TrimSpace(cfg.AdminPassword) != "" {
		admin, err = NewAdminHandler(cfg, logger, store, server.runs, metrics, webui.FS())
		if err != nil {
			logger.Fatalf("init admin handler: %v", err)
		}
		logger.Printf("admin WebUI enabled at /admin (data dir: %s)", cfg.DataDir)
	} else {
		logger.Printf("admin WebUI disabled (set ADMIN_PASSWORD to enable)")
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(admin),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		logger.Printf("listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("listen: %v", err)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http shutdown error: %v", err)
	}
	cancelRun()
	server.Shutdown(shutdownCtx)
}
