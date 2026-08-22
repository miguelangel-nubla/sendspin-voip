package http

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strings"
	"time"

	"github.com/miguelangel-nubla/sendspin-voip/internal/app"
	"github.com/miguelangel-nubla/sendspin-voip/internal/domain"
)

// ServerConfig holds HTTP server configuration parameters.
type ServerConfig struct {
	Listen      string
	APIToken    string
	EnablePprof bool
	Version     string
	Commit      string
	BuildDate   string
}

// Server provides an HTTP interface for web UI dashboard and debug/streams JSON APIs.
type Server struct {
	logger        *slog.Logger
	config        ServerConfig
	bridgeService *app.BridgeService
	sipCaller     app.SIPCallerPort
	startTime     time.Time
	httpServer    *http.Server
	stopCtx       context.Context
	stopCancel    context.CancelFunc
}

// NewServer creates a new HTTP server.
func NewServer(
	logger *slog.Logger,
	cfg ServerConfig,
	bridgeService *app.BridgeService,
	sipCaller app.SIPCallerPort,
) *Server {
	logger = cmp.Or(logger, slog.Default())
	cfg.Listen = cmp.Or(cfg.Listen, ":8080")
	cfg.Version = cmp.Or(cfg.Version, "dev")

	stopCtx, stopCancel := context.WithCancel(context.Background())

	s := &Server{
		logger:        logger,
		config:        cfg,
		bridgeService: bridgeService,
		sipCaller:     sipCaller,
		startTime:     time.Now(),
		stopCtx:       stopCtx,
		stopCancel:    stopCancel,
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	return s
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// UI Dashboard
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/streams", s.handleDashboard)

	// JSON APIs (go2rtc compatible style + pipeline inspector)
	mux.HandleFunc("/api/streams", s.handleAPIStreams)
	mux.HandleFunc("/api/events", s.handleAPIEvents)
	mux.HandleFunc("/api/info", s.handleAPIInfo)
	mux.HandleFunc("/api/status", s.handleAPIInfo)
	mux.HandleFunc("/api/codecs", s.handleAPICodecs)
	mux.HandleFunc("/metrics", s.handleMetrics)

	// Health and Readiness probes (Kubernetes / Docker)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/livez", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

	// Profiling — disabled by default (enable via http.enable_pprof)
	if s.config.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Time{})
			pprof.Profile(w, r)
		})
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Time{})
			pprof.Trace(w, r)
		})
	}
}

// authMiddleware enforces an optional API token on all routes when configured.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	token := strings.TrimSpace(s.config.APIToken)
	if token == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bypass token verification for container health check endpoints
		if r.URL.Path == "/healthz" || r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		provided := r.URL.Query().Get("token")
		if provided == "" {
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				provided = strings.TrimSpace(auth[7:])
			}
		}
		if provided == "" {
			provided = r.Header.Get("X-Api-Token")
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sendspin-voip"`)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start runs the HTTP listener in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.config.Listen)
	if err != nil {
		return fmt.Errorf("failed to listen on HTTP %s: %w", s.config.Listen, err)
	}

	s.logger.Info("HTTP server running and listening",
		"listen", s.config.Listen,
		"auth_enabled", strings.TrimSpace(s.config.APIToken) != "",
		"pprof_enabled", s.config.EnablePprof,
		"ui_url", fmt.Sprintf("http://localhost%s/", formatPortForURL(s.config.Listen)),
		"api_streams", fmt.Sprintf("http://localhost%s/api/streams", formatPortForURL(s.config.Listen)),
	)

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", "err", err)
		}
	}()

	return nil
}

func formatPortForURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	_, port, err := net.SplitHostPort(addr)
	if err == nil {
		return ":" + port
	}
	return ":8080"
}

// Shutdown stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Stopping HTTP server...")
	if s.stopCancel != nil {
		s.stopCancel()
	}
	return s.httpServer.Shutdown(ctx)
}

// handleAPIStreams returns streams information.
func (s *Server) handleAPIStreams(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	src := r.URL.Query().Get("src")
	if src != "" {
		info, ok := s.bridgeService.GetStreamDebugInfo(src)
		if !ok {
			http.Error(w, `{"error":"stream not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(info)
		return
	}

	streams := s.bridgeService.GetStreamsDebugInfo()

	if r.URL.Query().Get("format") == "list" {
		list := make([]app.StreamDebugInfo, 0, len(streams))
		for _, info := range streams {
			list = append(list, info)
		}
		_ = json.NewEncoder(w).Encode(list)
		return
	}

	_ = json.NewEncoder(w).Encode(streams)
}

// handleAPIEvents provides Server-Sent Events (SSE) for zero-latency live dashboard updates.
func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Disable HTTP server write deadline for this long-lived SSE stream
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sendUpdate := func() bool {
		streams := s.bridgeService.GetStreamsDebugInfo()
		data, err := json.Marshal(streams)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Send immediate initial update
	if !sendUpdate() {
		return
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
			if !sendUpdate() {
				return
			}
		}
	}
}

// SystemInfo represents runtime application status.
type SystemInfo struct {
	Version            string        `json:"version"`
	Commit             string        `json:"commit"`
	BuildDate          string        `json:"build_date"`
	GoVersion          string        `json:"go_version"`
	UptimeSec          float64       `json:"uptime_sec"`
	Goroutines         int           `json:"goroutines"`
	MemoryAllocMB      float64       `json:"memory_alloc_mb"`
	MemoryTotalAllocMB float64       `json:"memory_total_alloc_mb"`
	MemorySysMB        float64       `json:"memory_sys_mb"`
	SIP                app.SIPStatus `json:"sip"`
	ActiveStreams      int           `json:"active_streams"`
	TotalStreams       int           `json:"total_streams"`
}

// handleAPIInfo returns system status, memory stats, and SIP status.
func (s *Server) handleAPIInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	streams := s.bridgeService.GetStreamsDebugInfo()
	activeCount := 0
	for _, st := range streams {
		if st.IsActive() {
			activeCount++
		}
	}

	sipStatus := app.SIPStatus{}
	if s.sipCaller != nil {
		sipStatus = s.sipCaller.RegistrationStatus()
	}

	info := SystemInfo{
		Version:            s.config.Version,
		Commit:             s.config.Commit,
		BuildDate:          s.config.BuildDate,
		GoVersion:          runtime.Version(),
		UptimeSec:          time.Since(s.startTime).Seconds(),
		Goroutines:         runtime.NumGoroutine(),
		MemoryAllocMB:      float64(m.Alloc) / 1024 / 1024,
		MemoryTotalAllocMB: float64(m.TotalAlloc) / 1024 / 1024,
		MemorySysMB:        float64(m.Sys) / 1024 / 1024,
		SIP:                sipStatus,
		ActiveStreams:      activeCount,
		TotalStreams:       len(streams),
	}

	_ = json.NewEncoder(w).Encode(info)
}

// handleAPICodecs returns supported audio codecs dynamically from domain preferences.
func (s *Server) handleAPICodecs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	codecs := make([]map[string]any, 0, len(domain.DefaultCodecPreferences))
	for _, c := range domain.DefaultCodecPreferences {
		codecs = append(codecs, map[string]any{
			"codec":             string(c),
			"name":              c.FullName(),
			"sdp_name":          c.SDPEncodingName(),
			"rtp_clock_rate":    c.RTPClockRate(),
			"audio_sample_rate": c.SampleRate(),
			"payload_type":      c.PayloadType(),
			"bitrate_kbps":      c.DefaultBitrateKbps(),
			"channels":          c.DefaultChannels(),
			"description":       c.LongDescription(),
		})
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"codecs": codecs,
	})
}

// handleDashboard renders the modern live Web UI.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/streams" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

// handleMetrics serves Prometheus metrics for monitoring and alerting.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var sipStatus app.SIPStatus
	if s.sipCaller != nil {
		sipStatus = s.sipCaller.RegistrationStatus()
	}

	var streams map[string]app.StreamDebugInfo
	if s.bridgeService != nil {
		streams = s.bridgeService.GetStreamsDebugInfo()
	}

	WritePrometheusMetrics(
		w,
		s.config.Version,
		s.config.Commit,
		s.config.BuildDate,
		s.startTime,
		sipStatus,
		streams,
	)
}

// handleHealthz responds with basic process liveness.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"uptime_sec": time.Since(s.startTime).Seconds(),
		"version":    s.config.Version,
	})
}

// handleReadyz checks whether SIP and Bridge services are operational.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var sipStatus app.SIPStatus
	if s.sipCaller != nil {
		sipStatus = s.sipCaller.RegistrationStatus()
	}

	ready := true
	if strings.EqualFold(sipStatus.Mode, "pbx") && !sipStatus.Registered {
		ready = false
	}

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "not_ready",
			"sip_status": sipStatus,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ready",
		"sip_status": sipStatus,
	})
}


