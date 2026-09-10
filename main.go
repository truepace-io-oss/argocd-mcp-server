// Command argocd-mcp is an MCP (Model Context Protocol) server that lets an AI
// agent inspect and operate one or more Argo CD installations.
//
// Every instance is reached through the Kubernetes API server of the cluster it
// runs in — never through the Argo CD HTTP API — so remote instances need no
// ingress, no VPN and no Argo CD account: just a ServiceAccount token whose RBAC
// allows working with argoproj.io resources. Authorization is therefore entirely
// Kubernetes RBAC; the server performs none of its own.
//
// The HTTP (streamable) transport has no built-in authentication unless the
// auth block is configured. Run it behind TLS and either enable OIDC/static
// tokens or place it on an internal ingress. Never expose it unauthenticated.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/argocd"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/auth"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/config"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/instances"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/mcpserver"
	"github.com/truepace-io-oss/argocd-mcp-server/internal/metrics"
)

// version is set via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", os.Getenv("AMCP_CONFIG"), "path to the server config YAML (env: AMCP_CONFIG)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(*configPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)
	mcpserver.SetVersion(version)

	// Metrics: register the client-go request-metrics adapters before any client
	// is built, and publish build info.
	metrics.RegisterClientGo()
	metrics.SetBuildInfo(version)

	for _, w := range cfg.Warnings() {
		logger.Warn("config", "warning", w)
	}

	reg, err := instances.Build(cfg)
	if err != nil {
		return err
	}
	logger.Info("instance registry built", "instances", cfg.InstanceNames(), "default", cfg.DefaultInstance, "readOnly", cfg.ReadOnly)

	srv := mcpserver.New(reg, cfg)

	// Agent-facing authentication (independent of instance/cluster auth).
	authn, err := auth.Build(context.Background(), cfg.Auth)
	if err != nil {
		return err
	}
	logger.Info("agent authentication", "mode", authn.Description)

	// One MCP server value, reused for every session.
	mcpSrv := srv.MCPServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)

	mux := http.NewServeMux()
	// Only /mcp is authenticated; health probes stay open, and the
	// protected-resource metadata endpoint is public by spec.
	mux.Handle("/mcp", authn.Middleware(handler))
	if authn.MetadataHandler != nil {
		mux.Handle(authn.MetadataPath, authn.MetadataHandler)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Ready if the default instance is reachable. A degraded remote instance
		// must not fail readiness, so only the default is probed.
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := reg.Default().Ping(ctx); err != nil {
			http.Error(w, "default Argo CD instance unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Metrics server on a separate, unauthenticated port (never behind /mcp auth
	// or the public ingress), plus a periodic instance-reachability probe.
	var metricsSrv *http.Server
	if addr := cfg.MetricsAddr; addr != "" && addr != "off" {
		mmux := http.NewServeMux()
		mmux.Handle("/metrics", promhttp.Handler())
		metricsSrv = &http.Server{Addr: addr, Handler: mmux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logger.Info("metrics listening", "addr", addr, "endpoint", "/metrics")
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server", "err", err)
			}
		}()
		go probeInstances(ctx, reg)
		go probeWriteGuards(ctx, reg, logger)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr, "endpoint", "/mcp")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	return httpSrv.Shutdown(shutdownCtx)
}

// probeInstances periodically pings every instance and updates the reachability
// gauge, so amcp_instance_up reflects health independently of tool traffic.
func probeInstances(ctx context.Context, reg *instances.Registry) {
	probe := func() {
		for _, in := range reg.All() {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := in.Ping(pctx)
			cancel()
			metrics.SetInstanceUp(in.Name, err == nil)
		}
	}
	probe()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}

// probeWriteGuards periodically verifies that no instance's credentials can
// rewrite an Application's source, and publishes amcp_write_guard_active.
//
// It runs on a slower cycle than the reachability probe: each check is a
// dry-run write, so it is cheap but not free, and the property it measures
// changes only when someone changes RBAC or the admission policy.
func probeWriteGuards(ctx context.Context, reg *instances.Registry, logger *slog.Logger) {
	probe := func() {
		for _, in := range reg.All() {
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			state, reason, err := in.CheckWriteGuard(pctx)
			cancel()
			switch state {
			case argocd.GuardActive:
				metrics.SetWriteGuardActive(in.Name, true)
			case argocd.GuardMissing:
				metrics.SetWriteGuardActive(in.Name, false)
				logger.Warn("write guard MISSING — this instance's credentials can rewrite an Application's source; install the argocd-mcp write guard (ValidatingAdmissionPolicy) in that cluster",
					"instance", in.Name, "reason", reason)
			default:
				// Never report an unknown state as safe.
				metrics.ClearWriteGuard(in.Name)
				logger.Debug("write guard not probed", "instance", in.Name, "reason", reason, "err", err)
			}
		}
	}
	probe()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
