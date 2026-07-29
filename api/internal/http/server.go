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
	"strconv"
	"strings"
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

// Server is the API's HTTP surface. It exports only the metrics it owns — the
// requests it serves and the orchestrator calls it issues. Queue depth, active
// workflow counts and the fleet-wide runner heartbeat age are orchestrator-owned
// state derived from the database, and the API has no database access
// (PROJECT.md, "Service boundaries"). Those three names used to be registered
// here as gauges set to zero once at construction and never written again,
// which made an alert on them permanently unfireable; the orchestrator exports
// the real ones (`moirai/observability.py`). See issue #124.
type Server struct {
	cfg      Config
	mux      *http.ServeMux
	srv      *http.Server
	logger   *slog.Logger
	metrics  *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
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
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moirai_api_requests_total",
			Help: "HTTP requests served by the API, by method, matched route, and response status.",
		}, []string{"method", "route", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "moirai_api_request_duration_seconds",
			Help:    "HTTP request duration served by the API, by method and matched route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	s.metrics.MustRegister(s.requests, s.latency)
	s.metrics.MustRegister(orchestrator.Collectors()...)
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

// Handler returns the full request chain the server listens with, middleware
// included. Mux() returns the bare router, so anything that has to observe
// middleware behaviour — request IDs, security headers, request metrics — must
// go through this instead.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
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
			elapsed := time.Since(started)
			s.logger.Info("api request", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", elapsed.Milliseconds())
			// Recorded from the same measurements the log line already carries,
			// so the metrics cost is one atomic add and one histogram
			// observation on a request that has finished. Prometheus resolves
			// each label child under its own read lock; nothing here touches
			// server state, so no lock is added to the serving path.
			method, route := methodLabel(r.Method), routeLabel(r)
			s.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
			s.latency.WithLabelValues(method, route).Observe(elapsed.Seconds())
		}()
		next.ServeHTTP(recorded, r)
	})
}

// unmatchedRoute is the route label for a request no registered pattern
// matched. Collapsing every such request into one series is the point: the
// requested path is attacker-controlled and would otherwise be unbounded
// cardinality.
const unmatchedRoute = "unmatched"

// routeLabel reduces a served request to the ServeMux pattern that matched it,
// never to its raw path. net/http assigns Request.Pattern on the request itself
// before invoking the matched handler, so it is readable here once the chain
// has returned. The label set is therefore bounded by the number of registered
// routes: `/api/v1/projects/{project_id}` stays one series however many project
// IDs are requested.
func routeLabel(r *http.Request) string {
	pattern := r.Pattern
	if pattern == "" {
		return unmatchedRoute
	}
	// A pattern is "[METHOD ][HOST]/[PATH]" and the method travels in its own
	// label, so only the path template belongs here.
	if index := strings.LastIndex(pattern, " "); index >= 0 {
		pattern = pattern[index+1:]
	}
	if pattern == "" {
		return unmatchedRoute
	}
	return pattern
}

// methodLabel bounds the method label to the verbs the API registers routes
// for. A request line may carry any token as its method, so labelling with
// r.Method directly would let a client mint a new time series per request.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	default:
		return "other"
	}
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
