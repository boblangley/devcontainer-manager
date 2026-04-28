package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bob/devcontainer-manager/internal/config"
	"github.com/bob/devcontainer-manager/internal/dockerx"
	"github.com/bob/devcontainer-manager/internal/model"
	"github.com/gorilla/websocket"
)

type Gateway struct {
	docker      *dockerx.Client
	logger      *slog.Logger
	client      *http.Client
	mu          sync.Mutex
	pairing     map[string]pairingCode
	browserAuth map[string]browserToken
}

type pairingCode struct {
	Code      string
	EnvID     string
	ExpiresAt time.Time
}

type browserToken struct {
	Token     string
	EnvID     string
	ExpiresAt time.Time
}

func New(docker *dockerx.Client, logger *slog.Logger) *Gateway {
	return &Gateway{
		docker:      docker,
		logger:      logger,
		client:      &http.Client{Timeout: 3 * time.Second},
		pairing:     map[string]pairingCode{},
		browserAuth: map[string]browserToken{},
	}
}

func (g *Gateway) ReconcileContainer(ctx context.Context, container *model.Container, cfg config.Config, publicBaseURL string) {
	enabled := cfg.T3.Enabled
	if container.Rule != nil && container.Rule.T3.Enabled != nil {
		enabled = *container.Rule.T3.Enabled
	}
	container.T3.Enabled = enabled
	if !enabled {
		container.T3.Status = "disabled"
		return
	}
	if !container.Running {
		container.T3.Status = "stopped"
		return
	}
	if container.ContainerIP == "" {
		container.T3.Status = "unreachable"
		container.T3.Error = "container has no IP address"
		return
	}
	port := cfg.T3.Port
	if port == 0 {
		port = 3773
	}
	backend := "http://" + container.ContainerIP + ":" + strconv.Itoa(port)
	container.T3.BackendURL = backend
	container.T3.LastProbe = time.Now().UTC()
	envID, err := g.probe(ctx, backend)
	if err != nil {
		container.T3.Status = "not-ready"
		container.T3.Error = err.Error()
		return
	}
	if envID == "" {
		envID = container.ShortID
	}
	container.T3.EnvironmentID = envID
	container.T3.HTTPBaseURL = strings.TrimRight(publicBaseURL, "/") + "/env/" + envID + "/"
	container.T3.WSBaseURL = strings.Replace(container.T3.HTTPBaseURL, "http://", "ws://", 1)
	container.T3.Status = "ready"
	container.T3.Error = ""
	if cfg.T3.AutoIssueBackendToken && container.T3.BackendToken == "" {
		token, err := g.issueBackendToken(ctx, container.ID, container.WorkspaceFolder)
		if err != nil {
			container.T3.Status = "ready-no-token"
			container.T3.Error = err.Error()
			return
		}
		container.T3.BackendToken = token
	}
}

func (g *Gateway) GeneratePairingCode(envID string) (string, time.Time) {
	code := randomCode(18)
	expires := time.Now().UTC().Add(5 * time.Minute)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pairing[code] = pairingCode{Code: code, EnvID: envID, ExpiresAt: expires}
	return code, expires
}

func (g *Gateway) RefreshBackendToken(ctx context.Context, containerID, workspace string) (string, error) {
	return g.issueBackendToken(ctx, containerID, workspace)
}

func (g *Gateway) ServeEnv(w http.ResponseWriter, r *http.Request, env model.Environment, status model.T3Status, refreshToken func(context.Context) (string, error)) {
	rest := strings.TrimPrefix(r.URL.Path, "/env/"+env.ID)
	if rest == "" {
		rest = "/"
	}
	if rest == "/api/auth/bootstrap/bearer" && r.Method == http.MethodPost {
		g.handleBootstrap(w, r, env.ID)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		if !g.authorized(r, env.ID) {
			http.Error(w, "missing or invalid environment token", http.StatusUnauthorized)
			return
		}
		g.proxyWebsocket(w, r, rest, status, refreshToken)
		return
	}
	if !publicPath(rest) && !g.authorized(r, env.ID) {
		http.Error(w, "missing or invalid environment token", http.StatusUnauthorized)
		return
	}
	g.proxyHTTP(w, r, rest, status, refreshToken)
}

func (g *Gateway) probe(ctx context.Context, backend string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backend+"/.well-known/t3/environment", nil)
	if err != nil {
		return "", err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New(resp.Status)
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	for _, key := range []string{"id", "environmentId", "environmentID"} {
		if value, ok := body[key].(string); ok && value != "" {
			return value, nil
		}
	}
	return "", nil
}

func (g *Gateway) issueBackendToken(ctx context.Context, containerID, workspace string) (string, error) {
	if workspace == "" {
		workspace = "/workspace"
	}
	out, err := g.docker.Exec(ctx, containerID, []string{"env", "T3CODE_HOME=" + workspace, "t3", "auth", "session", "issue", "--role", "owner", "--label", "gateway", "--token-only"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (g *Gateway) handleBootstrap(w http.ResponseWriter, r *http.Request, envID string) {
	var payload map[string]string
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload)
	code := payload["code"]
	if code == "" {
		code = payload["pairingCode"]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.pairing[code]
	if !ok || entry.EnvID != envID || time.Now().After(entry.ExpiresAt) {
		http.Error(w, "invalid pairing code", http.StatusUnauthorized)
		return
	}
	delete(g.pairing, code)
	token := randomToken(32)
	expires := time.Now().UTC().Add(24 * time.Hour)
	g.browserAuth[token] = browserToken{Token: token, EnvID: envID, ExpiresAt: expires}
	writeJSON(w, http.StatusOK, map[string]any{
		"bearerToken": token,
		"token":       token,
		"tokenType":   "Bearer",
		"expiresAt":   expires,
	})
}

func (g *Gateway) authorized(r *http.Request, envID string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return false
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.browserAuth[token]
	if !ok || entry.EnvID != envID || time.Now().After(entry.ExpiresAt) {
		return false
	}
	return true
}

func (g *Gateway) proxyHTTP(w http.ResponseWriter, r *http.Request, rest string, status model.T3Status, refreshToken func(context.Context) (string, error)) {
	target, err := url.Parse(status.BackendURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	do := func(token string) (*http.Response, error) {
		upstreamURL := *target
		upstreamURL.Path = singleJoiningSlash(target.Path, rest)
		upstreamURL.RawQuery = r.URL.RawQuery
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = r.Header.Clone()
		req.Header.Del("Authorization")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return g.client.Do(req)
	}
	resp, err := do(status.BackendToken)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && refreshToken != nil {
		_ = resp.Body.Close()
		token, refreshErr := refreshToken(r.Context())
		if refreshErr == nil {
			resp, err = do(token)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (g *Gateway) proxyWebsocket(w http.ResponseWriter, r *http.Request, rest string, status model.T3Status, refreshToken func(context.Context) (string, error)) {
	wsToken, err := g.issueWSToken(r.Context(), status)
	if err != nil && refreshToken != nil {
		token, refreshErr := refreshToken(r.Context())
		if refreshErr == nil {
			status.BackendToken = token
			wsToken, err = g.issueWSToken(r.Context(), status)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	target, _ := url.Parse(status.BackendURL)
	target.Scheme = strings.Replace(target.Scheme, "http", "ws", 1)
	target.Path = singleJoiningSlash(target.Path, rest)
	query := r.URL.Query()
	query.Set("wsToken", wsToken)
	target.RawQuery = query.Encode()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	downstream, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer downstream.Close()
	upstream, _, err := websocket.DefaultDialer.DialContext(r.Context(), target.String(), nil)
	if err != nil {
		_ = downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, err.Error()))
		return
	}
	defer upstream.Close()
	errc := make(chan error, 2)
	go copyWS(errc, downstream, upstream)
	go copyWS(errc, upstream, downstream)
	<-errc
}

func (g *Gateway) issueWSToken(ctx context.Context, status model.T3Status) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(status.BackendURL, "/")+"/api/auth/ws-token", nil)
	if err != nil {
		return "", err
	}
	if status.BackendToken != "" {
		req.Header.Set("Authorization", "Bearer "+status.BackendToken)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New(resp.Status)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	for _, key := range []string{"wsToken", "token"} {
		if value, ok := payload[key].(string); ok && value != "" {
			return value, nil
		}
	}
	return "", errors.New("ws token response did not include a token")
}

func publicPath(rest string) bool {
	return rest == "/.well-known/t3/environment"
}

func copyWS(errc chan<- error, dst, src *websocket.Conn) {
	for {
		mt, msg, err := src.ReadMessage()
		if err != nil {
			errc <- err
			return
		}
		if err := dst.WriteMessage(mt, msg); err != nil {
			errc <- err
			return
		}
	}
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func singleJoiningSlash(a, b string) string {
	return path.Join("/", strings.TrimPrefix(a, "/"), strings.TrimPrefix(b, "/"))
}

func randomCode(n int) string {
	return strings.ToUpper(strings.TrimRight(base64.RawURLEncoding.EncodeToString(randomBytes(n)), "="))
}

func randomToken(n int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(n))
}

func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return buf
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
