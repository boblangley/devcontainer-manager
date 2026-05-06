package t3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
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
	docker      dockerClient
	logger      *slog.Logger
	client      *http.Client
	tokenStore  BrowserTokenStore
	mu          sync.Mutex
	pairing     map[string]pairingCode
	browserAuth map[string]browserToken
}

type dockerClient interface {
	Exec(ctx context.Context, containerID string, cmd []string, args ...string) (string, error)
	ExecWithStdin(ctx context.Context, containerID string, cmd []string, user string, input []byte) error
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

type SpawnStageRequest struct {
	Agent          string      `json:"agent"`
	RenderedPrompt string      `json:"renderedPrompt"`
	WorkspacePath  string      `json:"workspacePath"`
	Scope          RetainScope `json:"scope"`
}

type RetainScope struct {
	BankID string   `json:"bankId"`
	Tags   []string `json:"tags"`
}

type t3Project struct {
	ID                    string          `json:"id"`
	Title                 string          `json:"title"`
	WorkspaceRoot         string          `json:"workspaceRoot"`
	DefaultModelSelection json.RawMessage `json:"defaultModelSelection"`
}

type orchestrationSnapshot struct {
	Projects []t3Project `json:"projects"`
}

func New(docker *dockerx.Client, logger *slog.Logger) *Gateway {
	return NewWithTokenStore(docker, logger, nil)
}

func NewWithTokenStore(docker *dockerx.Client, logger *slog.Logger, tokenStore BrowserTokenStore) *Gateway {
	return newGateway(docker, logger, tokenStore)
}

func newGateway(docker dockerClient, logger *slog.Logger, tokenStore BrowserTokenStore) *Gateway {
	return &Gateway{
		docker:      docker,
		logger:      logger,
		client:      &http.Client{Timeout: 3 * time.Second},
		tokenStore:  tokenStore,
		pairing:     map[string]pairingCode{},
		browserAuth: map[string]browserToken{},
	}
}

func (g *Gateway) Close() {
	if closer, ok := g.tokenStore.(interface{ Close() }); ok {
		closer.Close()
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
		g.handleBootstrap(w, r, env)
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		if !g.authorizedWithStore(r, env.ID, firstNonEmpty(env.ContainerID, env.ID)) {
			http.Error(w, "missing or invalid environment token", http.StatusUnauthorized)
			return
		}
		g.proxyWebsocket(w, r, rest, status, refreshToken)
		return
	}
	if !publicPath(rest) && !g.authorizedWithStore(r, env.ID, firstNonEmpty(env.ContainerID, env.ID)) {
		http.Error(w, "missing or invalid environment token", http.StatusUnauthorized)
		return
	}
	g.proxyHTTP(w, r, rest, status, refreshToken)
}

func (g *Gateway) ServeSpawnStage(w http.ResponseWriter, r *http.Request, container model.Container, refreshToken func(context.Context) (string, error)) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.authorizedWithStore(r, container.T3.EnvironmentID, firstNonEmpty(container.ID, container.T3.EnvironmentID)) {
		http.Error(w, "missing or invalid environment token", http.StatusUnauthorized)
		return
	}
	var req SpawnStageRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid spawn request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateSpawnRequest(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scope, err := json.Marshal(req.Scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scope = append(scope, '\n')
	target := filepath.Join(req.WorkspacePath, ".hindsight", "active-retain-scope.json")
	user := firstNonEmpty(container.ContainerUser, "vscode")
	cmd := []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1.tmp" && mv "$1.tmp" "$1"`, "--", target}
	if err := g.docker.ExecWithStdin(r.Context(), container.ID, cmd, user, scope); err != nil {
		http.Error(w, "failed to write retain scope: "+err.Error(), http.StatusBadGateway)
		return
	}
	threadID, err := g.openStageThread(r.Context(), container.T3, refreshToken, req)
	if err != nil {
		http.Error(w, "failed to open t3 session: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"threadId": threadID})
}

func validateSpawnRequest(req SpawnStageRequest) error {
	switch req.Agent {
	case "codex", "claude", "opencode":
	default:
		return errors.New("agent must be codex, claude, or opencode")
	}
	if strings.TrimSpace(req.RenderedPrompt) == "" {
		return errors.New("renderedPrompt is required")
	}
	if req.WorkspacePath == "" || !filepath.IsAbs(req.WorkspacePath) {
		return errors.New("workspacePath must be an absolute container path")
	}
	if strings.TrimSpace(req.Scope.BankID) == "" {
		return errors.New("scope.bankId is required")
	}
	return nil
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

func (g *Gateway) handleBootstrap(w http.ResponseWriter, r *http.Request, env model.Environment) {
	var payload map[string]string
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload)
	code := payload["code"]
	if code == "" {
		code = payload["pairingCode"]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.pairing[code]
	if !ok || entry.EnvID != env.ID || time.Now().After(entry.ExpiresAt) {
		http.Error(w, "invalid pairing code", http.StatusUnauthorized)
		return
	}
	delete(g.pairing, code)
	token := randomToken(32)
	expires := time.Now().UTC().Add(24 * time.Hour)
	if g.tokenStore != nil {
		if err := g.tokenStore.Save(r.Context(), firstNonEmpty(env.ContainerID, env.ID), token, expires); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	g.deleteBrowserTokensLocked(env.ID)
	g.browserAuth[token] = browserToken{Token: token, EnvID: env.ID, ExpiresAt: expires}
	writeJSON(w, http.StatusOK, map[string]any{
		"bearerToken": token,
		"token":       token,
		"tokenType":   "Bearer",
		"expiresAt":   expires,
	})
}

func (g *Gateway) Authorized(r *http.Request, envID string) bool {
	return g.authorized(r, envID)
}

func (g *Gateway) authorized(r *http.Request, envID string) bool {
	return g.authorizedWithStore(r, envID, envID)
}

func (g *Gateway) authorizedWithStore(r *http.Request, envID, storeID string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return false
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	g.mu.Lock()
	entry, ok := g.browserAuth[token]
	if ok && entry.EnvID == envID && time.Now().Before(entry.ExpiresAt) {
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()
	if g.tokenStore == nil {
		return false
	}
	entry, ok, err := g.tokenStore.Get(r.Context(), storeID)
	if err != nil {
		g.logger.Warn("failed to load pairing token", "environment", envID, "error", err)
		return false
	}
	if !ok || entry.Token != token || time.Now().After(entry.ExpiresAt) {
		if ok && time.Now().After(entry.ExpiresAt) {
			_ = g.tokenStore.Delete(r.Context(), storeID)
		}
		return false
	}
	entry.EnvID = envID
	g.mu.Lock()
	g.browserAuth[token] = entry
	g.mu.Unlock()
	return true
}

func (g *Gateway) deleteBrowserTokensLocked(envID string) {
	for token, entry := range g.browserAuth {
		if entry.EnvID == envID {
			delete(g.browserAuth, token)
		}
	}
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

func (g *Gateway) openStageThread(ctx context.Context, status model.T3Status, refreshToken func(context.Context) (string, error), req SpawnStageRequest) (string, error) {
	var snapshot orchestrationSnapshot
	if err := g.backendJSON(ctx, &status, refreshToken, http.MethodGet, "/api/orchestration/snapshot", nil, &snapshot); err != nil {
		return "", err
	}
	modelSelection := modelSelectionFor(req.Agent, nil)
	project, ok := projectForWorkspace(snapshot.Projects, req.WorkspacePath)
	if ok && len(project.DefaultModelSelection) > 0 && string(project.DefaultModelSelection) != "null" {
		modelSelection = modelSelectionFor(req.Agent, project.DefaultModelSelection)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if !ok {
		project = t3Project{
			ID:            "dcm-project-" + randomToken(12),
			Title:         projectTitle(req.WorkspacePath),
			WorkspaceRoot: req.WorkspacePath,
		}
		if err := g.dispatch(ctx, &status, refreshToken, map[string]any{
			"type":                  "project.create",
			"commandId":             "dcm-command-" + randomToken(12),
			"projectId":             project.ID,
			"title":                 project.Title,
			"workspaceRoot":         project.WorkspaceRoot,
			"defaultModelSelection": modelSelection,
			"createdAt":             now,
		}); err != nil {
			return "", err
		}
	}
	threadID := "dcm-thread-" + randomToken(12)
	title := threadTitle(req.RenderedPrompt)
	if err := g.dispatch(ctx, &status, refreshToken, map[string]any{
		"type":            "thread.create",
		"commandId":       "dcm-command-" + randomToken(12),
		"threadId":        threadID,
		"projectId":       project.ID,
		"title":           title,
		"modelSelection":  modelSelection,
		"runtimeMode":     "full-access",
		"interactionMode": "default",
		"branch":          nil,
		"worktreePath":    nil,
		"createdAt":       now,
	}); err != nil {
		return "", err
	}
	if err := g.dispatch(ctx, &status, refreshToken, map[string]any{
		"type":      "thread.turn.start",
		"commandId": "dcm-command-" + randomToken(12),
		"threadId":  threadID,
		"message": map[string]any{
			"messageId":   "dcm-message-" + randomToken(12),
			"role":        "user",
			"text":        req.RenderedPrompt,
			"attachments": []any{},
		},
		"modelSelection":  modelSelection,
		"titleSeed":       title,
		"runtimeMode":     "full-access",
		"interactionMode": "default",
		"createdAt":       now,
	}); err != nil {
		return "", err
	}
	return threadID, nil
}

func (g *Gateway) dispatch(ctx context.Context, status *model.T3Status, refreshToken func(context.Context) (string, error), command map[string]any) error {
	var result map[string]any
	return g.backendJSON(ctx, status, refreshToken, http.MethodPost, "/api/orchestration/dispatch", command, &result)
}

func (g *Gateway) backendJSON(ctx context.Context, status *model.T3Status, refreshToken func(context.Context) (string, error), method, rest string, input, output any) error {
	do := func(token string) (*http.Response, error) {
		target := strings.TrimRight(status.BackendURL, "/") + rest
		var body io.Reader
		if input != nil {
			buf, err := json.Marshal(input)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, err
		}
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return g.client.Do(req)
	}
	resp, err := do(status.BackendToken)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && refreshToken != nil {
		_ = resp.Body.Close()
		token, refreshErr := refreshToken(ctx)
		if refreshErr == nil {
			status.BackendToken = token
			resp, err = do(token)
		}
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output)
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

func projectForWorkspace(projects []t3Project, workspace string) (t3Project, bool) {
	clean := filepath.Clean(workspace)
	for _, project := range projects {
		if filepath.Clean(project.WorkspaceRoot) == clean {
			return project, true
		}
	}
	return t3Project{}, false
}

func modelSelectionFor(agent string, existing json.RawMessage) map[string]any {
	instanceID := providerInstanceID(agent)
	if len(existing) > 0 {
		var selection map[string]any
		if err := json.Unmarshal(existing, &selection); err == nil {
			if selection["instanceId"] == instanceID {
				return selection
			}
		}
	}
	return map[string]any{
		"instanceId": instanceID,
		"model":      defaultModel(agent),
	}
}

func providerInstanceID(agent string) string {
	if agent == "claude" {
		return "claudeAgent"
	}
	return agent
}

func defaultModel(agent string) string {
	switch agent {
	case "claude":
		return "claude-sonnet-4-6"
	case "opencode":
		return "openai/gpt-5"
	default:
		return "gpt-5.4"
	}
}

func projectTitle(workspace string) string {
	base := filepath.Base(filepath.Clean(workspace))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "Workspace"
	}
	return base
}

func threadTitle(prompt string) string {
	title := strings.TrimSpace(strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")[0])
	if title == "" {
		return "Spawned stage"
	}
	const max = 80
	if len(title) > max {
		title = strings.TrimSpace(title[:max])
	}
	return title
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
