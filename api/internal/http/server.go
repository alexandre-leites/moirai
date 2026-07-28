package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/loop-engineering/api/internal/orchestrator"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	apiPathPrefix   = "/api/v1"
	requestIDHeader = "X-Request-ID"
)

type Config struct {
	BindAddress         string
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	ShutdownTimeout     time.Duration
	MaxRequestBodyBytes int64
	// OrchestratorHealthy reports whether the orchestrator gRPC connection is
	// reachable. When nil, health/readiness routes assume it is (used by tests
	// and any caller that has not wired an orchestrator client yet).
	OrchestratorHealthy func() bool
}

func DefaultConfig() Config {
	return Config{
		BindAddress:         ":8080",
		ReadTimeout:         30 * time.Second,
		WriteTimeout:        60 * time.Second,
		ShutdownTimeout:     15 * time.Second,
		MaxRequestBodyBytes: 1 << 20,
	}
}

func (c Config) Validate() error {
	if c.BindAddress == "" {
		return errors.New("server bind address is required")
	}
	if _, _, err := net.SplitHostPort(c.BindAddress); err != nil {
		return errors.New("server bind address is invalid")
	}
	if c.MaxRequestBodyBytes < 1024 {
		return errors.New("server request body limit is invalid")
	}
	return nil
}

type Server struct {
	cfg     Config
	mux     *http.ServeMux
	srv     *http.Server
	logger  *slog.Logger
	metrics *prometheus.Registry
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	s := &Server{
		cfg:     cfg,
		mux:     mux,
		logger:  logger,
		metrics: prometheus.NewRegistry(),
	}
	for _, gauge := range []prometheus.Gauge{
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "moirai_queue_depth", Help: "Eligible issue queue depth"}),
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "moirai_active_workflow_count", Help: "Active workflow count"}),
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "moirai_runner_heartbeat_age_seconds", Help: "Age of the oldest runner heartbeat"}),
	} {
		gauge.Set(0)
		s.metrics.MustRegister(gauge)
	}
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics, promhttp.HandlerOpts{}))
	srv := &http.Server{
		Addr:         cfg.BindAddress,
		Handler:      s.withMiddleware(mux),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	s.srv = srv
	s.registerHealthRoutes()
	return s, nil
}

func (s *Server) Mux() *http.ServeMux {
	return s.mux
}

func (s *Server) ListenAndServe() error {
	s.logger.Info("api server listening", "address", s.cfg.BindAddress)
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) registerHealthRoutes() {
	s.mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if s.orchestratorHealthy() {
			w.WriteHeader(http.StatusOK)
			return
		}
		WriteError(w, http.StatusServiceUnavailable, "Service unavailable", "orchestrator is unreachable")
	})
	s.mux.HandleFunc("GET "+apiPathPrefix+"/health", func(w http.ResponseWriter, r *http.Request) {
		orchestratorHealthy := s.orchestratorHealthy()
		status := http.StatusOK
		apiStatus := "healthy"
		orchestratorStatus := "reachable"
		if !orchestratorHealthy {
			status = http.StatusServiceUnavailable
			apiStatus = "degraded"
			orchestratorStatus = "unreachable"
		}
		WriteJSON(w, status, map[string]any{
			"status":       apiStatus,
			"orchestrator": orchestratorStatus,
		})
	})
}

func (s *Server) orchestratorHealthy() bool {
	if s.cfg.OrchestratorHealthy == nil {
		return true
	}
	return s.cfg.OrchestratorHealthy()
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(requestIDHeader)
		if requestID == "" || len(requestID) > 128 {
			requestID = newRequestID()
		}
		w.Header().Set(requestIDHeader, requestID)
		r = r.WithContext(orchestrator.WithRequestID(r.Context(), requestID))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		w.Header().Set("Referrer-Policy", "no-referrer")
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBodyBytes)
		started := time.Now()
		recorded := &statusRecorder{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("api request panic", "request_id", requestID, "method", r.Method, "path", r.URL.Path)
				WriteError(recorded, http.StatusInternalServerError, "Internal server error", "")
			}
			status := recorded.status
			if status == 0 {
				status = http.StatusOK
			}
			s.logger.Info("api request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds())
		}()
		next.ServeHTTP(recorded, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(value)
}

func newRequestID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(bytes)
}

type Problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

func WriteError(w http.ResponseWriter, status int, title string, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Status: status,
		Title:  title,
		Detail: detail,
	})
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
