package orchestrator_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/orchestrator"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// meteredControlPlane answers the two RPCs this test needs and leaves the rest
// to the generated Unimplemented base, which is itself a third outcome worth
// counting.
type meteredControlPlane struct {
	controlv1.UnimplementedControlPlaneServer
}

func (meteredControlPlane) WhoAmI(context.Context, *controlv1.WhoAmIRequest) (*controlv1.WhoAmIResponse, error) {
	return &controlv1.WhoAmIResponse{}, nil
}

func (meteredControlPlane) Login(context.Context, *controlv1.LoginRequest) (*controlv1.LoginResponse, error) {
	return nil, status.Error(codes.Unauthenticated, "no")
}

// TestOrchestratorCallsAreCountedByRpcAndStatus covers the API's other half of
// issue #124: the outcome of every orchestrator call it makes is now visible to
// a scrape, labelled by which RPC it was and how it ended.
func TestOrchestratorCallsAreCountedByRpcAndStatus(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	controlv1.RegisterControlPlaneServer(server, meteredControlPlane{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := orchestrator.Dial(ctx, listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// The counter is process-wide, so every assertion is a delta against what
	// was already recorded rather than an absolute value.
	before := callCounts(t)

	if _, err := client.WhoAmI(ctx); err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if _, err := client.Login(ctx, "user", "password"); err == nil {
		t.Fatal("Login was expected to be rejected")
	}
	if _, err := client.ListProjects(ctx); err == nil {
		t.Fatal("ListProjects was expected to be unimplemented")
	}
	if _, err := client.ListProjects(ctx); err == nil {
		t.Fatal("ListProjects was expected to be unimplemented")
	}

	after := callCounts(t)
	for series, want := range map[string]float64{
		`moirai_api_orchestrator_calls_total{code="OK",rpc="WhoAmI"}`:                  1,
		`moirai_api_orchestrator_calls_total{code="Unauthenticated",rpc="Login"}`:      1,
		`moirai_api_orchestrator_calls_total{code="Unimplemented",rpc="ListProjects"}`: 2,
	} {
		if delta := after[series] - before[series]; delta != want {
			t.Errorf("%s increased by %v, want %v", series, delta, want)
		}
	}
}

// The RPC label comes from the generated method name, so it is bounded by the
// proto service; the status label is a closed gRPC enum. Neither can be widened
// by a caller.
func TestOrchestratorCallLabelsStayBounded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := orchestrator.Dial(ctx, listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	if _, err := client.WhoAmI(ctx); err == nil {
		t.Fatal("WhoAmI against a bare server was expected to fail")
	}

	body := scrapeCollectors(t)
	examined := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "moirai_api_orchestrator_calls_total{") {
			continue
		}
		examined++
		labels := line[strings.Index(line, "{")+1 : strings.Index(line, "}")]
		for _, pair := range strings.Split(labels, ",") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				t.Fatalf("unparsable label pair %q", pair)
			}
			value = strings.Trim(value, `"`)
			switch key {
			case "rpc":
				if strings.ContainsAny(value, "/. ") {
					t.Errorf("rpc label %q is not a bare method name", value)
				}
			case "code":
				if !isStatusCodeName(value) {
					t.Errorf("code label %q is not a gRPC status code", value)
				}
			default:
				t.Errorf("unexpected label %q on the orchestrator call counter", key)
			}
		}
	}
	if examined == 0 {
		t.Fatalf("no orchestrator call series were exported:\n%s", body)
	}
}

// isStatusCodeName reports whether name is the String() form of one of gRPC's
// status codes, which is the closed set the code label may take.
func isStatusCodeName(name string) bool {
	for code := codes.OK; code <= codes.Unauthenticated; code++ {
		if code.String() == name {
			return true
		}
	}
	return false
}

// callCounts renders the orchestrator package's collectors and returns every
// series of the call counter by name.
func callCounts(t *testing.T) map[string]float64 {
	t.Helper()
	counts := map[string]float64{}
	for _, line := range strings.Split(scrapeCollectors(t), "\n") {
		if !strings.HasPrefix(line, "moirai_api_orchestrator_calls_total{") {
			continue
		}
		name, raw, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		counts[name] = value
	}
	return counts
}

func scrapeCollectors(t *testing.T) string {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(orchestrator.Collectors()...)
	response := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).
		ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}
