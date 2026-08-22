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
	"github.com/miguelangel-nubla/sendspin-voip/internal/version"
)

var (
	// Populated via -ldflags -X main.<var>=...
	Version   string
	Commit    string
	BuildDate string
)

func main() {
	configPath := flag.String("config", "", "Path to YAML configuration file")
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "Print version information and exit")
	flag.BoolVar(&showVersion, "v", false, "Print version information and exit (shorthand)")
	flag.Parse()

	ver, commit, buildDate := version.Resolve(Version, Commit, BuildDate)

	if showVersion || (flag.NArg() > 0 && flag.Arg(0) == "version") {
		fmt.Println(version.Info(ver, commit, buildDate))
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
		"version", ver,
		"commit", commit,
		"built", buildDate,
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
		Version:  ver,
	})

	arbiter := domain.NewTargetArbiter(cfg.Bridge.ConflictPolicy)
	logger.Info("Persisting player state", "path", cfg.StateFile)
	stateStore, err := state.NewFileStore(cfg.StateFile)
	if err != nil {
		logger.Warn("Failed to load existing player state from file", "path", cfg.StateFile, "err", err)
	}
	bridgeService := app.NewBridgeService(
		logger,
		cfg.Bridge,
		arbiter,
		sipCaller,
		rtpStreamer,
		ingress,
		stateStore,
	)

	// 4. Root Application Context with Signal Cancellation
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
			Version:     ver,
			Commit:      commit,
			BuildDate:   buildDate,
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
	<-ctx.Done()
	stop()

	logger.Info("Shutdown signal received, terminating active sessions...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)

	bridgeService.Shutdown()
	logger.Info("Shutdown complete. Goodbye!")
}
