// Command podbay is the Happy Pods server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/slopus/pods/internal/server"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "podbay: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("podbay", flag.ContinueOnError)
	addr := fs.String("addr", envDefault("PODBAY_ADDR", ":7777"), "listen address")
	dataDir := fs.String("data", envDefault("PODBAY_DATA", "./data"), "data directory")
	secretFlag := fs.String("secret", os.Getenv("PODBAY_SECRET"), "API bearer secret")
	publicURL := fs.String("public-url", os.Getenv("PODBAY_PUBLIC_URL"), "public base URL for generated site URLs")
	showVersion := fs.Bool("version", false, "print version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *showVersion {
		fmt.Println("podbay " + version)
		return nil
	}

	secret, generated, err := resolveSecret(*dataDir, *secretFlag)
	if err != nil {
		return err
	}
	if generated {
		fmt.Printf("generated secret: %s\n", secret)
	}

	app, err := server.New(server.Config{
		DataDir:   *dataDir,
		Secret:    secret,
		PublicURL: *publicURL,
	})
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(logger, app),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("podbay listening", "addr", *addr, "data", *dataDir)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("podbay shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func resolveSecret(dataDir, configured string) (secret string, generated bool, err error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		return configured, false, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, err
	}
	path := filepath.Join(dataDir, "secret")
	if data, err := os.ReadFile(path); err == nil {
		secret := strings.TrimSpace(string(data))
		if secret != "" {
			return secret, false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}

	secret, err = generateSecret()
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return "", false, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", false, err
	}
	return secret, true, nil
}

func generateSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"duration", time.Since(start).String(),
		)
	})
}
