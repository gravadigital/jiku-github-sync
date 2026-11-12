// Command jiku-github-sync keeps the requirements of Jiku projects in sync with the issues of their
// GitHub repos and Projects. See README.md.
//
//	jiku-github-sync [serve]                        run the worker (default)
//	jiku-github-sync check-config [<project>]       compare the mapping with GitHub
//	jiku-github-sync sync-inicial <project> [--dry-run]
//	jiku-github-sync verify <project>               compare both sides, read-only
//	jiku-github-sync healthcheck                    query /readyz (Docker HEALTHCHECK)
//	jiku-github-sync version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	jikugo "github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"

	"github.com/gravadigital/jiku-github-sync/internal/checkconfig"
	"github.com/gravadigital/jiku-github-sync/internal/config"
	"github.com/gravadigital/jiku-github-sync/internal/engine"
	"github.com/gravadigital/jiku-github-sync/internal/github"
	"github.com/gravadigital/jiku-github-sync/internal/health"
	"github.com/gravadigital/jiku-github-sync/internal/initial"
	"github.com/gravadigital/jiku-github-sync/internal/jiku"
	"github.com/gravadigital/jiku-github-sync/internal/logging"
	"github.com/gravadigital/jiku-github-sync/internal/mapping"
	"github.com/gravadigital/jiku-github-sync/internal/state"
	"github.com/gravadigital/jiku-github-sync/internal/verify"
	"github.com/gravadigital/jiku-github-sync/internal/webhook"
	"github.com/gravadigital/jiku-github-sync/internal/worker"
)

// version is set at build time (-ldflags "-X main.version=1.0.0").
var version = "dev"

const usage = `jiku-github-sync — Jiku ↔ GitHub (Issues + Projects) connector

  jiku-github-sync [serve]                    runs the worker (default)
  jiku-github-sync check-config [<project>]   compares the mapping with GitHub: repos, Project
                                              fields and options, issue types (exit 1 on errors)
  jiku-github-sync sync-inicial <project> [--dry-run]
                                              one-time sync of everything open that already
                                              exists, both ways, with its comments
  jiku-github-sync verify <project>           read-only: compares both sides of every pair and
                                              lists what is not linked (exit 1 on differences)
  jiku-github-sync healthcheck                queries /readyz of the local process
  jiku-github-sync version

Configuration is read from the environment (see .env.dist) and the mapping from SYNC_CONFIG
(see config.example.yaml).`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	cfg := config.FromEnv()
	log := logging.New(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, cfg, log)
	case "check-config":
		err = checkConfig(ctx, cfg, log, args)
	case "sync-inicial", "initial-sync":
		err = syncInicial(ctx, cfg, log, args)
	case "verify":
		err = verifyCmd(ctx, cfg, log, args)
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

func newGitHub(cfg config.Config, log *slog.Logger) (*github.Client, error) {
	if err := cfg.ValidateGitHub(); err != nil {
		return nil, err
	}
	return github.New(github.Options{BaseURL: cfg.GitHubAPIURL, Token: cfg.GitHubToken, WriteInterval: cfg.WriteInterval, Logger: log}), nil
}

// connect opens the bus as the connector's machine user and waits until core knows it: on the
// very first connection core has no row for the identity yet and refuses everything with
// caller_not_authorized until the auth event is processed.
func connect(ctx context.Context, cfg config.Config, log *slog.Logger) (*jikugo.Client, error) {
	if err := cfg.ValidateJiku(); err != nil {
		return nil, err
	}
	key, err := cfg.ServiceKey()
	if err != nil {
		return nil, err
	}
	src, err := auth.NewServiceUser(auth.ServiceUserConfig{Issuer: cfg.JikuIssuer, Key: key, ProjectID: cfg.JikuProjectID})
	if err != nil {
		return nil, fmt.Errorf("machine user key: %w", err)
	}
	backoff := 2 * time.Second
	for attempt := 1; ; attempt++ {
		c, err := jikugo.Connect(ctx, jikugo.Config{
			Servers: cfg.JikuServers, Instance: cfg.JikuInstance, Creds: cfg.JikuCreds, Timeout: cfg.JikuTimeout,
			Name: "jiku-github-sync", Auth: src,
		})
		if err != nil {
			return nil, fmt.Errorf("connecting to the Jiku bus: %w", err)
		}
		_, err = c.List(ctx, "projects", jikugo.List{Limit: 1, Fields: []string{"id"}})
		if err == nil {
			log.Info("connected to the Jiku bus", "server", c.ConnectedURL(), "userId", c.UserID())
			return c, nil
		}
		_ = c.Close()
		unknownYet := jikugo.IsCode(err, jikugo.CodeCallerNotAuthorized) || jikugo.IsCode(err, "unknown_caller")
		if attempt >= 6 || !unknownYet {
			return nil, fmt.Errorf("the connector's identity cannot query Jiku: %w", err)
		}
		log.Warn("Jiku does not know this identity yet; reconnecting", "attempt", attempt)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

func newStore(ctx context.Context, cfg config.Config, c *jikugo.Client) (*state.Store, error) {
	if cfg.StateBackend == "memory" {
		return state.New(state.NewMem()), nil
	}
	kv, err := c.KV(ctx)
	if err != nil {
		return nil, fmt.Errorf("the connector's KV space: %w", err)
	}
	return state.New(state.JikuKV{S: kv}), nil
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	m, err := mapping.Load(cfg.MappingFile)
	if err != nil {
		return err
	}
	gh, err := newGitHub(cfg, log)
	if err != nil {
		return err
	}
	login, err := gh.Viewer(ctx)
	if err != nil {
		return fmt.Errorf("GitHub token: %w", err)
	}
	log.Info("github identity", "login", login)

	h := health.New()
	mux := h.Mux()
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	h.Add("jiku-bus", func(context.Context) error { return errors.New("connecting") })
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	c, err := connect(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer c.Close()
	h.Add("jiku-bus", func(context.Context) error {
		if !c.Conn().IsConnected() {
			return errors.New("disconnected")
		}
		return nil
	})
	st, err := newStore(ctx, cfg, c)
	if err != nil {
		return err
	}
	jk := jiku.New(c)
	eng := engine.New(engine.Options{GitHub: gh, Jiku: jk, Store: st, Config: m, Logger: log})
	w := worker.New(worker.Options{
		Engine: eng, GitHub: gh, Jiku: jk, Store: st, Logger: log,
		Events:               worker.JikuEvents{Client: c, Durable: cfg.EventsDurable},
		PollInterval:         cfg.PollInterval,
		ReconcileInterval:    cfg.ReconcileInterval,
		MappingFile:          cfg.MappingFile,
		ConfigReload:         cfg.ConfigReload,
		WriteInterval:        cfg.WriteInterval,
		VerifyAfterReconcile: cfg.VerifyAfterReconcile,
	})
	h.Add("github-poll", w.Ready)
	if cfg.WebhookSecret != "" {
		mux.Handle("POST /webhooks/github", &webhook.Handler{Secret: []byte(cfg.WebhookSecret), Log: log, Enqueue: func(t webhook.Trigger) {
			if t.Node != "" {
				w.Enqueue(worker.Key{Kind: worker.IssueNode, Node: t.Node})
				return
			}
			w.Enqueue(worker.Key{Kind: worker.Issue, Repo: t.Repo, Number: t.Number})
		}})
		log.Info("github webhook receiver enabled", "path", "/webhooks/github")
	}
	log.Info("started", "version", version, "projects", len(m.Projects), "httpAddr", cfg.HTTPAddr, "state", cfg.StateBackend)

	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	}
	log.Info("shutting down")
	<-done
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func checkConfig(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	m, err := mapping.Load(cfg.MappingFile)
	if err != nil {
		return err
	}
	gh, err := newGitHub(cfg, log)
	if err != nil {
		return err
	}
	var only int64
	if len(args) > 0 {
		if only, err = strconv.ParseInt(args[0], 10, 64); err != nil {
			return fmt.Errorf("project id: %w", err)
		}
		if m.Project(only) == nil {
			return fmt.Errorf("project %d is not in %s", only, cfg.MappingFile)
		}
	}
	fmt.Printf("Mapeo %s: válido (%d proyectos)\n", cfg.MappingFile, len(m.Projects))
	if n := checkconfig.Render(os.Stdout, checkconfig.Run(ctx, gh, m, only)); n > 0 {
		return fmt.Errorf("%d errors in the mapping", n)
	}
	return nil
}

func syncInicial(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("sync-inicial", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print the plan and write nothing")
	// Accept the project id before or after the flags.
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	if len(pos) != 1 {
		return errors.New("usage: jiku-github-sync sync-inicial <project> [--dry-run]")
	}
	projectID, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return fmt.Errorf("project id: %w", err)
	}
	m, err := mapping.Load(cfg.MappingFile)
	if err != nil {
		return err
	}
	p := m.Project(projectID)
	if p == nil {
		return fmt.Errorf("project %d is not in %s", projectID, cfg.MappingFile)
	}
	gh, err := newGitHub(cfg, log)
	if err != nil {
		return err
	}
	c, err := connect(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer c.Close()
	st, err := newStore(ctx, cfg, c)
	if err != nil {
		return err
	}
	jk := jiku.New(c)
	pl, err := initial.Build(ctx, initial.Deps{GitHub: gh, Jiku: jk, Store: st, WriteInterval: cfg.WriteInterval, ItemSettle: engine.DefaultItemSettle}, p)
	if err != nil {
		return err
	}
	pl.Render(os.Stdout)
	if *dry {
		fmt.Println("\n--dry-run: no se escribió nada.")
		return nil
	}
	fmt.Println("\nAplicando…")
	eng := engine.New(engine.Options{GitHub: gh, Jiku: jk, Store: st, Config: m, Logger: log})
	if err := initial.Apply(ctx, eng, st, pl, os.Stdout); err != nil {
		return fmt.Errorf("the initial sync finished with errors (running it again is safe): %w", err)
	}
	fmt.Println("Listo.")
	return nil
}

func verifyCmd(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: jiku-github-sync verify <project>")
	}
	projectID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("project id: %w", err)
	}
	m, err := mapping.Load(cfg.MappingFile)
	if err != nil {
		return err
	}
	p := m.Project(projectID)
	if p == nil {
		return fmt.Errorf("project %d is not in %s", projectID, cfg.MappingFile)
	}
	gh, err := newGitHub(cfg, log)
	if err != nil {
		return err
	}
	c, err := connect(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer c.Close()
	st, err := newStore(ctx, cfg, c)
	if err != nil {
		return err
	}
	rep, err := verify.Run(ctx, gh, gh, jiku.New(c), st, p)
	if err != nil {
		return err
	}
	return rep.Render(os.Stdout)
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
