// Command jiku-github-sync keeps the requirements of Jiku projects in sync with the issues of their
// GitHub repos and Projects. See README.md.
//
//	jiku-github-sync [serve]      run the worker (default)
//	jiku-github-sync healthcheck  query /readyz of the local process (Docker HEALTHCHECK)
//	jiku-github-sync version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gravadigital/jiku-github-sync/internal/config"
	"github.com/gravadigital/jiku-github-sync/internal/health"
	"github.com/gravadigital/jiku-github-sync/internal/logging"
)

// version is set at build time (-ldflags "-X main.version=1.0.0").
var version = "dev"

const usage = `jiku-github-sync — Jiku ↔ GitHub (Issues + Projects) connector

  jiku-github-sync [serve]       runs the worker (default)
  jiku-github-sync healthcheck   queries /readyz of the local process (for the Docker HEALTHCHECK)
  jiku-github-sync version

Configuration is read from the environment: see .env.dist.`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	cfg := config.FromEnv()
	log := logging.New(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, cfg, log)
	case "healthcheck":
		err = healthcheck(cfg)
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

// serve runs the worker until SIGINT/SIGTERM. For now it only exposes the health endpoints; the
// Jiku bus, the GitHub reader and the sync engine are added step by step.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	h := health.New()
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: h.Mux(), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	log.Info("started", "version", version, "httpAddr", cfg.HTTPAddr)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func healthcheck(cfg config.Config) error {
	addr := cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: HTTP %d", resp.StatusCode)
	}
	return nil
}
