package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const version = "0.0.3-dev"

// Run starts a local bootstrap service until the context is cancelled.
func Run(ctx context.Context, portVariable string) error {
	port, err := portFromEnv(portVariable)
	if err != nil {
		return err
	}

	address := net.JoinHostPort("127.0.0.1", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	log.Printf("bootstrap service listening on http://%s", listener.Addr())
	return serve(ctx, listener)
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

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(struct {
			Status    string `json:"status"`
			Timestamp int64  `json:"timestamp"`
			Version   string `json:"version"`
		}{"healthy", time.Now().UnixMilli(), version}); err != nil {
			log.Printf("write health response: %v", err)
		}
	})
	return mux
}

func serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{
		Handler:           handler(),
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
func Main(portVariable string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, portVariable); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
