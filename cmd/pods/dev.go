package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/slopus/pods/internal/server"
)

var devSiteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// cmdDev runs a local dev server: it serves a directory live and provides the
// same `/api/db` JSON store API (backed by in-memory SQLite) and `/pods.js`
// client that production does, with no login, deploy, or files on disk.
func cmdDev(args []string) error {
	fs := flag.NewFlagSet("pods dev", flag.ContinueOnError)
	addr := fs.String("addr", ":7777", "listen address")
	name := fs.String("name", "", "site name (defaults to pods.json, then the directory name)")
	openBrowser := fs.Bool("open", false, "open the dev URL in your browser")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// Allow "pods dev <dir> --addr :8080".
	rest := fs.Args()
	dir := "."
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		dir = rest[0]
		if err := parseFlags(fs, rest[1:]); err != nil {
			return err
		}
		rest = fs.Args()
	}
	if len(rest) > 0 {
		return errors.New("usage: pods dev [dir] [--addr :7777] [--name N] [--open]")
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absDir); err != nil || !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}

	resolved, err := resolveSiteName(*name, dir)
	if err != nil {
		return err
	}
	siteName := sanitizeSiteName(resolved)

	app, err := server.NewDev(siteName, absDir)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
	}

	url := devURL(*addr)
	fmt.Printf("pods dev — serving %s as site %q\n", dir, siteName)
	fmt.Printf("  %s\n", url)
	fmt.Printf("  static files served live from %s\n", absDir)
	fmt.Printf("  JSON store API at %s/api/db (in-memory; resets on exit), client at %s/pods.js\n", url, url)
	fmt.Println("  press Ctrl+C to stop")
	if *openBrowser && runtime.GOOS == "darwin" {
		_ = exec.Command("open", url).Start()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\npods dev: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// sanitizeSiteName coerces an arbitrary name into a DNS-label site name,
// falling back to "dev" when nothing usable remains.
func sanitizeSiteName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	if len(cleaned) > 63 {
		cleaned = strings.Trim(cleaned[:63], "-")
	}
	if !devSiteNameRe.MatchString(cleaned) {
		return "dev"
	}
	return cleaned
}

// devURL renders a browser URL for the listen address.
func devURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://localhost:7777"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}
