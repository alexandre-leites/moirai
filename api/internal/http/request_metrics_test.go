package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	apiserver "github.com/loop-engineering/api/internal/http"
)

func newMeteredServer(t *testing.T) *apiserver.Server {
	t.Helper()
	// A discarding logger keeps the deliberate panic below out of the test
	// output without suppressing the middleware behaviour under test.
	server, err := apiserver.New(apiserver.DefaultConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return server
}

// TestRequestCounterCountsEveryServedRequest is the acceptance test for the
// API's side of issue #124: after two requests the counter for that route and
// status reads 2, where the metrics this endpoint used to export could only
// ever read zero.
func TestRequestCounterCountsEveryServedRequest(t *testing.T) {
	server := newMeteredServer(t)

	for range 2 {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/live", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /live = %d, want 200", response.Code)
		}
	}

	body := scrapeMetrics(t, server)
	series := `moirai_api_requests_total{method="GET",route="/live",status="200"}`
	if value := metricValue(t, body, series); value != 2 {
		t.Fatalf("%s = %v, want 2", series, value)
	}
	if value := metricValue(t, body, `moirai_api_request_duration_seconds_count{method="GET",route="/live"}`); value != 2 {
		t.Fatalf("request duration observations = %v, want 2", value)
	}
}

func TestRequestMetricsSeparateStatusesAndMethods(t *testing.T) {
	server := newMeteredServer(t)
	server.Mux().HandleFunc("POST /api/v1/metrics-test/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	for _, id := range []string{"one", "two", "three"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/metrics-test/"+id, nil))
		if response.Code != http.StatusAccepted {
			t.Fatalf("POST returned %d, want 202", response.Code)
		}
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/no-such-route", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unmatched GET = %d, want 404", response.Code)
	}

	body := scrapeMetrics(t, server)
	// Three distinct paths, one series: the label is the route template, so
	// path parameters cannot multiply the cardinality of the metric.
	matched := `moirai_api_requests_total{method="POST",route="/api/v1/metrics-test/{id}",status="202"}`
	if value := metricValue(t, body, matched); value != 3 {
		t.Errorf("%s = %v, want 3", matched, value)
	}
	unmatched := `moirai_api_requests_total{method="GET",route="unmatched",status="404"}`
	if value := metricValue(t, body, unmatched); value != 1 {
		t.Errorf("%s = %v, want 1", unmatched, value)
	}
}

// A request path is chosen by the caller, so labelling with it would let anyone
// mint unbounded series against the API's own metrics endpoint. Both the route
// and the method have to collapse instead.
func TestRequestMetricsDoNotGrowWithCallerControlledInput(t *testing.T) {
	server := newMeteredServer(t)

	// Warm the two series an unmatched request produces before counting, so the
	// comparison measures growth rather than first appearance.
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/warm-up", nil))
	server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("BREW", "/warm-up", nil))
	before := seriesCount(scrapeMetrics(t, server), "moirai_api_requests_total")

	for _, path := range []string{"/a", "/b", "/c", "/api/v1/deep/nested/unknown", "/api/v1/projects/" + strings.Repeat("x", 200)} {
		server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	for _, method := range []string{"BREW", "WHATEVER", "PROPFIND"} {
		server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/a", nil))
	}

	body := scrapeMetrics(t, server)
	if after := seriesCount(body, "moirai_api_requests_total"); after != before {
		t.Errorf("request counter series grew from %d to %d; the labels are caller-controlled", before, after)
	}
	if value := metricValue(t, body, `moirai_api_requests_total{method="other",route="unmatched",status="404"}`); value != 4 {
		t.Errorf("unknown-method bucket = %v, want 4", value)
	}
}

// A handler that panics is still a served request, and the 500 the middleware
// writes for it is exactly the status an operator wants counted.
func TestRequestMetricsRecordPanickingHandlers(t *testing.T) {
	server := newMeteredServer(t)
	server.Mux().HandleFunc("GET /api/v1/metrics-panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/metrics-panic", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panicking handler returned %d, want 500", response.Code)
	}

	series := `moirai_api_requests_total{method="GET",route="/api/v1/metrics-panic",status="500"}`
	if value := metricValue(t, scrapeMetrics(t, server), series); value != 1 {
		t.Errorf("%s = %v, want 1", series, value)
	}
}

func scrapeMetrics(t *testing.T, server *apiserver.Server) string {
	t.Helper()
	response := httptest.NewRecorder()
	server.Mux().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func metricValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, raw, ok := strings.Cut(line, " ")
		if !ok || name != series {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return value
	}
	t.Fatalf("series %q is not exported:\n%s", series, body)
	return 0
}

func seriesCount(body, name string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+"{") {
			count++
		}
	}
	return count
}
