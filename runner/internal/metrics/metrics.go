package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	server       *http.Server
	heartbeatAge prometheus.Gauge
}

func New(bind string) *Server {
	registry := prometheus.NewRegistry()
	for _, gauge := range []prometheus.Gauge{
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "moirai_queue_depth", Help: "Eligible issue queue depth"}),
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "moirai_active_workflow_count", Help: "Active workflow count"}),
	} {
		gauge.Set(0)
		registry.MustRegister(gauge)
	}
	heartbeatAge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "moirai_runner_heartbeat_age_seconds", Help: "Age of the last runner heartbeat"})
	heartbeatAge.Set(0)
	registry.MustRegister(heartbeatAge)
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return &Server{server: &http.Server{Addr: bind, Handler: mux, ReadHeaderTimeout: 5 * time.Second}, heartbeatAge: heartbeatAge}
}

func (s *Server) Start() {
	go func() { _ = s.server.ListenAndServe() }()
}

func (s *Server) MarkHeartbeat() {
	s.heartbeatAge.Set(0)
}
