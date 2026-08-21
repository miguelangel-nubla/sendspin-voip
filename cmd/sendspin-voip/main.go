package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/audio"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/config"
	infraHttp "github.com/miguelangel-nubla/sendspin-voip/internal/infra/http"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/rtp"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/sendspin"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/sip"
	"github.com/miguelangel-nubla/sendspin-voip/internal/infra/state"
)

var (
	// Injected at build time via -ldflags
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func main() {
	configPath := flag.String("config", "", "Path to YAML configuration file")
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("sendspin-voip version %s (commit: %s, built: %s)\n", Version, Commit, BuildDate)
		os.Exit(0)
	}

	// 1. Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	// 2. Setup Structured Logger
	var logLevel slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	logger.Info("Starting sendspin-voip",
		"version", Version,
		"commit", Commit,
		"built", BuildDate,
	)

	// 3. Domain & Infrastructure Dependency Injection
	rtpStreamer := rtp.NewStreamer(logger, func() app.AudioTranscoderPort {
		return audio.NewTranscoder()
	}, cfg.SIP.RTPPortMin, cfg.SIP.RTPPortMax)

	sipCaller, err := sip.NewCaller(logger, sip.CallerConfig{
		Mode:                   cfg.SIP.Mode,
		Server:                 cfg.SIP.Server,
		Username:               cfg.SIP.Username,
		Password:               cfg.SIP.Password,
		Domain:                 cfg.SIP.Domain,
		Transport:              cfg.SIP.Transport,
		LocalIP:                cfg.SIP.LocalIP,
		LocalSIPPort:           cfg.SIP.LocalSIPPort,
		AutoAnswerPreset:       cfg.SIP.AutoAnswerPreset,
		CustomAutoAnswerHeader: cfg.SIP.CustomAutoAnswerHeader,
	})
	if err != nil {
		logger.Error("Failed to create SIP caller", "err", err)
		os.Exit(1)
	}

	ingress := sendspin.NewIngress(logger, sendspin.IngressConfig{
		Server:   cfg.Sendspin.Server,
		BufferMs: cfg.Sendspin.BufferMs,
	})

	arbiter := domain.NewTargetArbiter(cfg.Bridge.ConflictPolicy)
	logger.Info("Persisting player state", "path", cfg.StateFile)
	stateStore := state.NewFileStore(cfg.StateFile)
	bridgeService := app.NewBridgeService(
		logger,
		cfg.ToBridgeConfig(),
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
		stateStore,
	)

	// 4. Start SIP Stack
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sipCaller.Start(ctx); err != nil {
		logger.Error("Failed to start SIP user agent", "err", err)
		os.Exit(1)
	}

	// 5. Register virtual Sendspin players
	domainPlayers, err := cfg.ToDomainPlayerConfigs()
	if err != nil {
		logger.Error("Invalid player configurations", "err", err)
		os.Exit(1)
	}

	if err := bridgeService.RegisterPlayers(domainPlayers); err != nil {
		logger.Error("Failed to register players", "err", err)
		os.Exit(1)
	}

	// 6. Start HTTP Server for UI and Streams Debug API
	httpServer := infraHttp.NewServer(
		logger,
		infraHttp.ServerConfig{
			Listen:      cfg.HTTP.Listen,
			APIToken:    cfg.HTTP.APIToken,
			EnablePprof: cfg.HTTP.EnablePprof,
			Version:     Version,
			Commit:      Commit,
			BuildDate:   BuildDate,
		},
		bridgeService,
		sipCaller,
	)

	if err := httpServer.Start(); err != nil {
		logger.Error("Failed to start HTTP server", "err", err)
		os.Exit(1)
	}

	logger.Info("sendspin-voip is running and ready for audio streams")

	// 7. Wait for Termination Signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutdown signal received, terminating active sessions...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)

	bridgeService.Shutdown()
	logger.Info("Shutdown complete. Goodbye!")
}
