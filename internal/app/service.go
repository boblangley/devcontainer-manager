package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bob/devcontainer-manager/internal/config"
	"github.com/bob/devcontainer-manager/internal/dockerx"
	"github.com/bob/devcontainer-manager/internal/model"
	"github.com/bob/devcontainer-manager/internal/syncx"
	"github.com/bob/devcontainer-manager/internal/t3"
	"github.com/fsnotify/fsnotify"
)

//go:embed web
var webFS embed.FS

type Service struct {
	configPath string
	cfg        atomic.Value
	docker     *dockerx.Client
	t3         *t3.Gateway
	sync       *syncx.Controller
	logger     *slog.Logger

	mu         sync.RWMutex
	containers map[string]model.Container
	sessions   []model.Session
}

func New(configPath string, cfg config.Config, logger *slog.Logger) (*Service, error) {
	docker, err := dockerx.New(logger)
	if err != nil {
		return nil, err
	}
	service := &Service{
		configPath: configPath,
		docker:     docker,
		t3:         t3.New(docker, logger),
		sync:       syncx.New(docker, logger),
		logger:     logger,
		containers: map[string]model.Container{},
	}
	service.cfg.Store(cfg)
	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	defer s.docker.Close()
	if err := s.reconcile(ctx); err != nil {
		s.logger.Warn("initial reconcile failed", "error", err)
	}
	server := &http.Server{Addr: s.config().Server.Listen, Handler: s.routes()}
	errc := make(chan error, 3)
	go func() {
		s.logger.Info("HTTP server listening", "listen", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go s.watchDocker(ctx, errc)
	go s.watchConfig(ctx, errc)
	go s.periodicReconcile(ctx)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errc:
		_ = server.Close()
		return err
	}
}

func (s *Service) reconcile(ctx context.Context) error {
	cfg := s.config()
	containers, err := s.docker.ListDevcontainers(ctx, cfg)
	if err != nil {
		return err
	}
	publicBase := publicBaseURL(cfg.Server.Listen)
	s.mu.RLock()
	previous := make(map[string]model.Container, len(s.containers))
	for id, container := range s.containers {
		previous[id] = container
	}
	s.mu.RUnlock()
	for index := range containers {
		if old, ok := previous[containers[index].ID]; ok {
			containers[index].T3.BackendToken = old.T3.BackendToken
		}
		s.t3.ReconcileContainer(ctx, &containers[index], cfg, publicBase)
	}
	sessions := s.sync.Reconcile(ctx, containers, cfg)
	applySyncSummaries(containers, sessions, cfg.Sync.Enabled)

	next := map[string]model.Container{}
	for _, container := range containers {
		next[container.ID] = container
	}
	s.mu.Lock()
	s.containers = next
	s.sessions = sessions
	s.mu.Unlock()
	s.logger.Info("reconciled containers", "containers", len(containers), "sessions", len(sessions))
	return nil
}

func (s *Service) watchDocker(ctx context.Context, errc chan<- error) {
	events, errs := s.docker.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(2 * time.Second)
				events, errs = s.docker.Events(ctx)
				continue
			}
			s.logger.Info("docker event", "action", event.Action, "container", event.ID)
			reconcileCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_ = s.reconcile(reconcileCtx)
			cancel()
		case err := <-errs:
			if err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("docker event stream error", "error", err)
				time.Sleep(2 * time.Second)
				events, errs = s.docker.Events(ctx)
			}
		}
	}
}

func (s *Service) watchConfig(ctx context.Context, errc chan<- error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.logger.Warn("config watcher unavailable", "error", err)
		return
	}
	defer watcher.Close()
	dir := filepath.Dir(s.configPath)
	if err := watcher.Add(dir); err != nil {
		s.logger.Warn("failed to watch config directory", "path", dir, "error", err)
		return
	}
	var timer *time.Timer
	for {
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			return
		case event := <-watcher.Events:
			if filepath.Clean(event.Name) == filepath.Clean(s.configPath) {
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(250 * time.Millisecond)
			}
		case err := <-watcher.Errors:
			s.logger.Warn("config watcher error", "error", err)
		case <-timerC:
			timer = nil
			cfg, err := config.Load(s.configPath)
			if err != nil {
				s.logger.Warn("config reload failed", "error", err)
				continue
			}
			s.cfg.Store(cfg)
			s.logger.Info("config reloaded", "path", s.configPath)
			reconcileCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_ = s.reconcile(reconcileCtx)
			cancel()
		}
	}
}

func (s *Service) periodicReconcile(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_ = s.reconcile(reconcileCtx)
			cancel()
		}
	}
}

func (s *Service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/containers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"containers": s.containerList()})
	})
	mux.HandleFunc("GET /api/environments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"environments": s.environments()})
	})
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sessions": s.sessionList()})
	})
	mux.HandleFunc("POST /api/environments/", s.handleEnvironmentAction)
	mux.HandleFunc("/env/", s.handleEnvProxy)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return withLogging(s.logger, mux)
}

func (s *Service) handleEnvironmentAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/environments/")
	envID, action, ok := strings.Cut(rest, "/")
	if !ok || action != "pairing-code" {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.environment(envID); !ok {
		http.NotFound(w, r)
		return
	}
	code, expires := s.t3.GeneratePairingCode(envID)
	writeJSON(w, http.StatusOK, map[string]any{"environmentId": envID, "pairingCode": code, "expiresAt": expires})
}

func (s *Service) handleEnvProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/env/")
	envID, _, _ := strings.Cut(rest, "/")
	env, ok := s.environment(envID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	container, ok := s.containerByEnv(envID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if container.T3.BackendURL == "" {
		http.Error(w, "environment backend is not ready", http.StatusBadGateway)
		return
	}
	refresh := func(ctx context.Context) (string, error) {
		token, err := s.t3.RefreshBackendToken(ctx, container.ID, container.WorkspaceFolder)
		if err != nil {
			return "", err
		}
		s.mu.Lock()
		current := s.containers[container.ID]
		current.T3.BackendToken = token
		s.containers[container.ID] = current
		s.mu.Unlock()
		return token, nil
	}
	s.t3.ServeEnv(w, r, env, container.T3, refresh)
}

func (s *Service) config() config.Config {
	return s.cfg.Load().(config.Config)
}

func (s *Service) containerList() []model.Container {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Container, 0, len(s.containers))
	for _, container := range s.containers {
		out = append(out, container)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Service) sessionList() []model.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]model.Session(nil), s.sessions...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Service) environments() []model.Environment {
	containers := s.containerList()
	out := make([]model.Environment, 0, len(containers))
	for _, container := range containers {
		if container.T3.EnvironmentID == "" {
			continue
		}
		out = append(out, envFromContainer(container))
	}
	return out
}

func (s *Service) environment(id string) (model.Environment, bool) {
	for _, env := range s.environments() {
		if env.ID == id {
			return env, true
		}
	}
	return model.Environment{}, false
}

func (s *Service) containerByEnv(id string) (model.Container, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, container := range s.containers {
		if container.T3.EnvironmentID == id {
			return container, true
		}
	}
	return model.Container{}, false
}

func envFromContainer(container model.Container) model.Environment {
	return model.Environment{
		ID:          container.T3.EnvironmentID,
		ContainerID: container.ID,
		Name:        container.Name,
		HTTPBaseURL: container.T3.HTTPBaseURL,
		WSBaseURL:   container.T3.WSBaseURL,
		Status:      container.T3.Status,
		Error:       container.T3.Error,
		LastProbe:   container.T3.LastProbe,
	}
}

func applySyncSummaries(containers []model.Container, sessions []model.Session, enabled bool) {
	for index := range containers {
		if !enabled || containers[index].Rule == nil || len(containers[index].Rule.Syncs) == 0 {
			containers[index].Sync = model.SyncSummary{Enabled: false, Status: "disabled"}
			continue
		}
		summary := model.SyncSummary{Enabled: true, Status: "ready"}
		for _, session := range sessions {
			if session.ContainerID != containers[index].ID {
				continue
			}
			summary.Sessions++
			if session.Error != "" {
				summary.Status = "degraded"
				if summary.Error == "" {
					summary.Error = session.Error
				}
			} else if session.Status != "" && session.Status != "ready" {
				summary.Status = session.Status
			}
		}
		if summary.Sessions == 0 {
			summary.Status = "idle"
		}
		containers[index].Sync = summary
	}
}

func publicBaseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://localhost:8787"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "127.0.0.1" || host == "::1" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start).String())
	})
}
