package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPortFromEnv(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"1", "1"},
		{"65535", "65535"},
		{"005301", "5301"},
		{"", ""},
		{"0", ""},
		{"65536", ""},
		{"-1", ""},
		{"+1", ""},
		{" 123", ""},
		{"abc", ""},
		{strings.Repeat("9", 100), ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Setenv("TEST_PORT", tc.input)
			got, err := portFromEnv("TEST_PORT")
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "TEST_PORT") {
					t.Fatalf("portFromEnv() = %q, %v; want named error", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("portFromEnv() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func mustHandler(t *testing.T, component string) http.Handler {
	t.Helper()
	h, err := handler(component)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHealth(t *testing.T) {
	for _, component := range []string{"RE", "PE"} {
		t.Run(component, func(t *testing.T) { testHealth(t, mustHandler(t, component)) })
	}
}

func TestUnknownComponent(t *testing.T) {
	if _, err := handler("XX"); err == nil {
		t.Fatal("handler accepted an unknown component")
	}
}

func testHealth(t *testing.T, h http.Handler) {
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("health response = %d, %q", response.Code, response.Header().Get("Content-Type"))
	}
	var body struct {
		Status    string `json:"status"`
		Timestamp int64  `json:"timestamp"`
		Version   string `json:"version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "healthy" || body.Version != version || time.Since(time.UnixMilli(body.Timestamp)).Abs() > time.Minute {
		t.Fatalf("unexpected health body: %+v", body)
	}

	unknown := httptest.NewRecorder()
	h.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", unknown.Code)
	}
}

func TestServeStopsOnCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, mustHandler(t, "RE")) }()

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/api/health")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("health = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
}

func TestRunReportsOccupiedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PORT", port)
	if err := Run(context.Background(), "RE", "TEST_PORT"); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Run with occupied port = %v", err)
	}
}

func TestUnsupportedAndUnknownRoutes(t *testing.T) {
	for _, tc := range []struct {
		component, method, path string
		status                  int
		surface                 string
	}{
		{"RE", http.MethodGet, "/api/arbitration", http.StatusNotImplemented, "GET /api/arbitration"},
		{"RE", http.MethodPost, "/api/engine/clone/machine/m-1", http.StatusNotImplemented, "POST /api/engine/clone/machine/:machineId"},
		{"RE", http.MethodGet, "/", http.StatusNotImplemented, "GET /"},
		{"RE", http.MethodPost, "/api/runtime/snapshot", http.StatusNotImplemented, "POST /api/runtime/snapshot"},
		{"PE", http.MethodGet, "/api/events", http.StatusNotImplemented, "SSE /api/events"},
		{"PE", http.MethodGet, "/api/arbitration", http.StatusNotFound, ""},
		{"RE", http.MethodGet, "/api/not-specified", http.StatusNotFound, ""},
		{"RE", http.MethodDelete, "/api/health", http.StatusNotFound, ""},
	} {
		t.Run(tc.component+" "+tc.method+" "+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mustHandler(t, tc.component).ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", response.Code, tc.status, response.Body)
			}
			var body map[string]string
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if tc.surface != "" && (body["surface"] != tc.surface || body["participation"] != "unsupported") {
				t.Fatalf("body = %v", body)
			}
			if body["error"] == "" {
				t.Fatalf("no error message: %v", body)
			}
		})
	}
}
