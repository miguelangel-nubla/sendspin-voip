package http

import (
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
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}

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

	writeTimeout := 10 * time.Second
	if cfg.EnablePprof {
		writeTimeout = 0
	}

	s.httpServer = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      writeTimeout,
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

	// Profiling — disabled by default (enable via http.enable_pprof)
	if s.config.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
}

// authMiddleware enforces an optional API token on all routes when configured.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	token := strings.TrimSpace(s.config.APIToken)
	if token == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Access-Control-Allow-Origin", "*")

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
	w.Header().Set("Access-Control-Allow-Origin", "*")

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
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	streams := s.bridgeService.GetStreamsDebugInfo()
	activeCount := 0
	for _, st := range streams {
		if st.State == "playing" || st.State == "active" || st.State == "dialing" || st.State == "lingering" {
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

// handleAPICodecs returns supported audio codecs.
func (s *Server) handleAPICodecs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	codecs := []map[string]any{
		{
			"codec":             "opus",
			"name":              "Opus Interactive Audio",
			"sdp_name":          "opus/48000/2",
			"rtp_clock_rate":    48000,
			"audio_sample_rate": 48000,
			"payload_type":      96,
			"bitrate_kbps":      128,
			"channels":          2,
			"description":       "High fidelity multi-room audio with zero-copy passthrough when volume is 100%",
		},
		{
			"codec":             "l16",
			"name":              "L16 Linear PCM (Uncompressed)",
			"sdp_name":          "L16/48000/1",
			"rtp_clock_rate":    48000,
			"audio_sample_rate": 48000,
			"payload_type":      97,
			"bitrate_kbps":      768,
			"channels":          1,
			"description":       "Studio master uncompressed linear PCM audio streaming",
		},
		{
			"codec":             "g722",
			"name":              "G.722 HD Voice",
			"sdp_name":          "G722/8000",
			"rtp_clock_rate":    8000,
			"audio_sample_rate": 16000,
			"payload_type":      9,
			"bitrate_kbps":      64,
			"channels":          1,
			"description":       "Wideband HD VoIP codec with crystal clear speech synthesis",
		},
		{
			"codec":             "pcmu",
			"name":              "G.711 µ-law (PCMU)",
			"sdp_name":          "PCMU/8000",
			"rtp_clock_rate":    8000,
			"audio_sample_rate": 8000,
			"payload_type":      0,
			"bitrate_kbps":      64,
			"channels":          1,
			"description":       "Universal standard telephony codec (North America/Japan standard)",
		},
		{
			"codec":             "pcma",
			"name":              "G.711 A-law (PCMA)",
			"sdp_name":          "PCMA/8000",
			"rtp_clock_rate":    8000,
			"audio_sample_rate": 8000,
			"payload_type":      8,
			"bitrate_kbps":      64,
			"channels":          1,
			"description":       "Universal standard telephony codec (International/European standard)",
		},
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
