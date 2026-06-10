// Command hermes is the VPS transport/status authority.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/legrin-tech/hermes/internal/activityhub"
	"github.com/legrin-tech/hermes/internal/agentstore"
	"github.com/legrin-tech/hermes/internal/envelopestore"
	"github.com/legrin-tech/hermes/internal/httpapi"
	"github.com/legrin-tech/hermes/internal/keystore"
	"github.com/legrin-tech/hermes/internal/notifystore"
	"github.com/legrin-tech/hermes/internal/projectstore"
	"github.com/legrin-tech/hermes/internal/sessionstore"
	"github.com/legrin-tech/hermes/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := ":8080"
	if v := os.Getenv("HERMES_ADDR"); v != "" {
		addr = v
	}
	dsn := "hermes.db"
	if v := os.Getenv("HERMES_DB"); v != "" {
		dsn = v
	}
	forgeURL := "http://127.0.0.1:8090"
	if v := os.Getenv("FORGE_URL"); v != "" {
		forgeURL = v
	}
	// Optional additional executor targets routed by envelope.target_node.
	// When set, MARSHAL_VPS_URL receives envelopes with target_node="marshal-vps"
	// and MARSHAL_MAC_URL receives target_node="marshal-mac". Both speak the
	// Forge /deliver wire protocol (see internal/worker/multitarget.go).
	marshalVPSURL := os.Getenv("MARSHAL_VPS_URL")
	marshalMacURL := os.Getenv("MARSHAL_MAC_URL")
	webhookURL := os.Getenv("HERMES_WEBHOOK_URL")
	webhookSecret := os.Getenv("HERMES_WEBHOOK_SECRET")
	tgToken := os.Getenv("HERMES_TG_TOKEN")
	tgChat := os.Getenv("HERMES_TG_CHAT")
	tgThread := os.Getenv("HERMES_TG_THREAD")
	iconsDir := os.Getenv("HERMES_ICONS_DIR")

	// CON-013: Agent TTL and prune interval with safe parsing.
	cfg := ParseAgentConfig(os.Getenv, func(msg string, args ...any) { logger.Warn(msg, args...) })
	agentTTL := cfg.TTL
	pruneInterval := cfg.Interval

	// signal.NotifyContext gives us a ctx that cancels on SIGINT/SIGTERM.
	// The HTTP server and the delivery worker both respect it, so a single
	// Ctrl-C drains both cleanly — W-H14 relies on this: the worker must
	// not be mid-flip when the process exits.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := envelopestore.Open(ctx, dsn)
	if err != nil {
		logger.Error("store open failed", "err", err, "dsn", dsn)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	// Share the same *sql.DB to avoid "database is locked" under concurrent
	// writes (review feedback: two independent pools against one DSN is unsafe).
	projects, err := projectstore.OpenWithDB(ctx, store.DB())
	if err != nil {
		logger.Error("project store open failed", "err", err)
		os.Exit(1)
	}

	notifications, err := notifystore.OpenWithDB(ctx, store.DB())
	if err != nil {
		logger.Error("notify store open failed", "err", err)
		os.Exit(1)
	}

	sessions, err := sessionstore.OpenWithDB(ctx, store.DB())
	if err != nil {
		logger.Error("session store open failed", "err", err)
		os.Exit(1)
	}

	keys, err := keystore.OpenWithDB(ctx, store.DB())
	if err != nil {
		logger.Error("keystore open failed", "err", err)
		os.Exit(1)
	}

	hub := activityhub.New()
	agents := agentstore.New(agentTTL)

	// CON-013: Start the agent prune ticker.
	StartPruneTicker(agents, pruneInterval, logger, ctx)

	// Seed icon_paths from deploy/icons directory (idempotent).
	if iconsDir != "" {
		if err := seedIconPaths(ctx, projects, iconsDir, logger); err != nil {
			logger.Error("icon seed failed", "err", err)
		}
	}

	srv := &http.Server{
		Addr: addr,
		Handler: httpapi.NewServer(logger, store, projects, notifications, sessions, httpapi.ServerOpts{
			Keys:          keys,
			Activity:      hub,
			Agents:        agents,
			WebhookURL:    webhookURL,
			WebhookSecret: webhookSecret,
			TGToken:       tgToken,
			TGChat:        tgChat,
			TGThread:      tgThread,
			IconsDir:      iconsDir,
		}),
	}

	defaultForge := worker.NewHTTPForgeClient(forgeURL)
	var deliveryClient worker.ForgeClient = defaultForge
	if marshalVPSURL != "" || marshalMacURL != "" {
		targets := map[string]worker.ForgeClient{}
		if marshalVPSURL != "" {
			targets["marshal-vps"] = worker.NewHTTPForgeClient(marshalVPSURL)
			logger.Info("multitarget: marshal-vps registered", "url", marshalVPSURL)
		}
		if marshalMacURL != "" {
			targets["marshal-mac"] = worker.NewHTTPForgeClient(marshalMacURL)
			logger.Info("multitarget: marshal-mac registered", "url", marshalMacURL)
		}
		deliveryClient = worker.NewMultiTarget(defaultForge, targets)
	}

	w := &worker.Worker{
		Store:    store,
		Client:   deliveryClient,
		Projects: projects,
		Tick:     500 * time.Millisecond,
		Logger:   logger,
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		logger.Info("hermes http starting", "addr", addr, "dsn", dsn)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server exited", "err", err)
			stop() // trip the shutdown path if the HTTP server dies first
		}
	}()

	go func() {
		defer wg.Done()
		logger.Info("hermes worker starting", "forge_url", forgeURL)
		if err := w.Run(ctx); err != nil {
			logger.Error("worker exited", "err", err)
		}
	}()

	<-ctx.Done()
	logger.Info("hermes shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "err", err)
	}
	wg.Wait()
	logger.Info("hermes stopped")
}

// seedIconPaths populates icon_path for projects that have matching files in iconsDir.
// It checks for both .png and .svg extensions.
func seedIconPaths(ctx context.Context, projects *projectstore.Store, iconsDir string, logger *slog.Logger) error {
	entries, err := os.ReadDir(iconsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := filepath.Ext(name)
		if ext != ".png" && ext != ".svg" {
			continue
		}
		project := name[:len(name)-len(ext)]
		iconPath := "/icons/" + name
		if err := projects.SetIconPath(ctx, project, iconPath); err != nil {
			if errors.Is(err, projectstore.ErrNotFound) {
				logger.Debug("icon file exists but project not registered", "project", project, "icon", name)
				continue
			}
			return err
		}
		logger.Info("seeded icon_path", "project", project, "icon", iconPath)
	}
	return nil
}
