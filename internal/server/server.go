package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jateeter/RealityEngine_GO/internal/engine"
	"github.com/jateeter/RealityEngine_GO/internal/instance"
	"github.com/jateeter/RealityEngine_GO/internal/lanetls"
	"github.com/jateeter/RealityEngine_GO/internal/pe"
	"github.com/jateeter/RealityEngine_GO/internal/surface"
)

const version = "0.0.3-dev"

// Run starts a component's service (surface.RE or surface.PE) until the
// context is cancelled.
func Run(ctx context.Context, component, portVariable string) error {
	port, err := portFromEnv(portVariable)
	if err != nil {
		return err
	}
	// The lane's TLS material is read first: a partial or unreadable set
	// refuses to start, and never falls back to plaintext.
	material, err := lanetls.FromEnvironment()
	if err != nil {
		return err
	}
	var serverTLS *tls.Config
	if material != nil {
		if serverTLS, err = material.Server(); err != nil {
			return err
		}
	}
	h, err := handler(component)
	if err != nil {
		return err
	}

	address := net.JoinHostPort("127.0.0.1", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	scheme := "http"
	if serverTLS != nil {
		listener, scheme = tls.NewListener(listener, serverTLS), "https"
	}
	log.Printf("bootstrap service listening on %s://%s", scheme, listener.Addr())
	return serve(ctx, listener, h)
}

func portFromEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s must be set to a port from 1 to 65535", name)
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("%s must be a port from 1 to 65535, got %q", name, value)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("%s must be a port from 1 to 65535, got %q", name, value)
	}
	return strconv.Itoa(port), nil
}

// handler serves the component's implemented routes and answers every other
// route the specification defines for it with the 501 unsupported envelope
// (SURFACE_SPEC "An unimplemented route answers unsupported"). Any other path
// is a plain 404.
func handler(component string) (http.Handler, error) {
	layers := surface.Layers(component)
	if layers == nil {
		return nil, fmt.Errorf("unknown component %q", component)
	}
	inv, err := surface.Load()
	if err != nil {
		return nil, err
	}
	health := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(struct {
			Status    string `json:"status"`
			Timestamp int64  `json:"timestamp"`
			Version   string `json:"version"`
		}{"healthy", time.Now().UnixMilli(), version}); err != nil {
			log.Printf("write health response: %v", err)
		}
	})
	handlers := map[string]http.Handler{}
	// contractRoutes are routes another contract defines and SURFACE_SPEC.md
	// does not list, keyed "METHOD /path": the PE's GET /api/metrics
	// (docs/PE_METRICS_CONTRACT.md). They are served ahead of the router.
	contractRoutes := map[string]http.Handler{}
	// The participant clock this process ticks on every receive and send
	// (RealityEngine_CI#600): the RE's instance clock, or the PE's own.
	var clock *instance.Clock
	switch component {
	case surface.RE:
		handlers["RE GET /api/health"] = health
		eng, err := engine.FromEnvironment()
		if err != nil {
			return nil, err
		}
		clock = eng.InstanceClock()
		// The K-line store's budget warnings, when there are any: an unexported
		// population is kept, never dropped, and the store still writes, so
		// status stays (SURFACE_SPEC "The K-line store").
		handlers["RE GET /api/health"] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body := map[string]any{"status": "healthy", "timestamp": time.Now().UnixMilli(), "version": version}
			if warnings := eng.KlineWarnings(); len(warnings) > 0 {
				body["warnings"] = warnings
			}
			writeJSON(w, http.StatusOK, body)
		})
		for id, h := range machineRoutes(eng) {
			handlers[id] = h
		}
	case surface.PE:
		handlers["PE GET /api/health"] = health
		material, err := lanetls.FromEnvironment()
		if err != nil {
			return nil, err
		}
		var clientTLS *tls.Config
		if material != nil {
			if clientTLS, err = material.Client(); err != nil {
				return nil, err
			}
		}
		perception, err := pe.FromEnvironment(clientTLS)
		if err != nil {
			return nil, err
		}
		clock = perception.Clock()
		for id, h := range peRoutes(perception) {
			handlers[id] = h
		}
		contractRoutes["GET /api/metrics"] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if _, err := io.WriteString(w, perception.Metrics()); err != nil {
				log.Printf("write metrics response: %v", err)
			}
		})
	}
	router, err := surface.NewRouter(inv, layers, surface.Implemented, handlers)
	if err != nil {
		return nil, err
	}
	// Anything else, including a specified path with a method the
	// specification doesn't give it, is a plain 404 with the error envelope.
	router.NotFound = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "Not found"}); err != nil {
			log.Printf("write not-found response: %v", err)
		}
	})
	if len(contractRoutes) == 0 {
		return instance.Middleware(clock, router), nil
	}
	return instance.Middleware(clock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := contractRoutes[r.Method+" "+r.URL.Path]; ok {
			h.ServeHTTP(w, r)
			return
		}
		router.ServeHTTP(w, r)
	})), nil
}

func serve(ctx context.Context, listener net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(listener)
	}()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			closeErr := srv.Close()
			return fmt.Errorf("shut down HTTP server: %w", errors.Join(err, closeErr))
		}
		err := <-done
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
		return nil
	}
}

// Main runs a bootstrap service and reports startup or shutdown errors.
func Main(component, portVariable string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, component, portVariable); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
