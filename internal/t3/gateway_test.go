package t3

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bob/devcontainer-manager/internal/model"
)

type fakeDocker struct {
	containerID string
	cmd         []string
	user        string
	input       []byte
	err         error
}

func (f *fakeDocker) Exec(context.Context, string, []string, ...string) (string, error) {
	return "", nil
}

func (f *fakeDocker) ExecWithStdin(_ context.Context, containerID string, cmd []string, user string, input []byte) error {
	f.containerID = containerID
	f.cmd = append([]string{}, cmd...)
	f.user = user
	f.input = append([]byte{}, input...)
	return f.err
}

type fakeTokenStore struct {
	tokens map[string]browserToken
}

func (s *fakeTokenStore) Save(_ context.Context, envID, token string, expiresAt time.Time) error {
	if s.tokens == nil {
		s.tokens = map[string]browserToken{}
	}
	s.tokens[envID] = browserToken{Token: token, EnvID: envID, ExpiresAt: expiresAt}
	return nil
}

func (s *fakeTokenStore) Get(_ context.Context, envID string) (browserToken, bool, error) {
	token, ok := s.tokens[envID]
	return token, ok, nil
}

func (s *fakeTokenStore) Delete(_ context.Context, envID string) error {
	delete(s.tokens, envID)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBrowserTokenPersistsThroughStore(t *testing.T) {
	store := &fakeTokenStore{}
	gateway := newGateway(&fakeDocker{}, testLogger(), store)
	code, _ := gateway.GeneratePairingCode("env-1")
	req := httptest.NewRequest(http.MethodPost, "/env/env-1/api/auth/bootstrap/bearer", strings.NewReader(`{"code":"`+code+`"}`))
	rec := httptest.NewRecorder()
	gateway.ServeEnv(rec, req, model.Environment{ID: "env-1", ContainerID: "container-1"}, model.T3Status{}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	token := body["bearerToken"]
	if token == "" {
		t.Fatal("bootstrap response did not include bearerToken")
	}
	if _, ok := store.tokens["container-1"]; !ok {
		t.Fatal("token was not stored by devcontainer id")
	}

	restarted := newGateway(&fakeDocker{}, testLogger(), store)
	authReq := httptest.NewRequest(http.MethodGet, "/env/env-1/api/auth/session", nil)
	authReq.Header.Set("Authorization", "Bearer "+token)
	if !restarted.authorizedWithStore(authReq, "env-1", "container-1") {
		t.Fatal("stored token did not authorize after gateway restart")
	}
}

func TestServeSpawnStageWritesScopeAndDispatchesT3Thread(t *testing.T) {
	var commands []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer stale-token" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer backend-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/orchestration/snapshot":
			writeJSON(w, http.StatusOK, map[string]any{"projects": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/orchestration/dispatch":
			var command map[string]any
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
				t.Errorf("decode command: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			commands = append(commands, command)
			writeJSON(w, http.StatusOK, map[string]any{"sequence": len(commands)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	docker := &fakeDocker{}
	gateway := newGateway(docker, testLogger(), nil)
	gateway.browserAuth["browser-token"] = browserToken{
		Token:     "browser-token",
		EnvID:     "env-1",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	body := `{
		"agent":"codex",
		"renderedPrompt":"Fix the auth bug",
		"workspacePath":"/workspace/auth-service",
		"scope":{"bankId":"comp:auth-service","tags":["card:ENG-103","type:bug"]}
	}`
	req := httptest.NewRequest(http.MethodPost, "/env/env-1/spawn-stage", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer browser-token")
	rec := httptest.NewRecorder()
	container := model.Container{
		ID:            "container-1",
		ContainerUser: "vscode",
		T3: model.T3Status{
			EnvironmentID: "env-1",
			BackendURL:    backend.URL,
			BackendToken:  "stale-token",
		},
	}
	refreshes := 0
	gateway.ServeSpawnStage(rec, req, container, func(context.Context) (string, error) {
		refreshes++
		return "backend-token", nil
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("spawn status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if refreshes != 1 {
		t.Fatalf("backend token refreshes = %d, want 1", refreshes)
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["threadId"] == "" {
		t.Fatal("spawn response did not include threadId")
	}
	if docker.containerID != "container-1" || docker.user != "vscode" {
		t.Fatalf("exec target = (%q, %q), want container-1/vscode", docker.containerID, docker.user)
	}
	wantTarget := "/workspace/auth-service/.hindsight/active-retain-scope.json"
	if !reflect.DeepEqual(docker.cmd, []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1.tmp" && mv "$1.tmp" "$1"`, "--", wantTarget}) {
		t.Fatalf("exec cmd = %#v", docker.cmd)
	}
	var scope map[string]any
	if err := json.Unmarshal(docker.input, &scope); err != nil {
		t.Fatal(err)
	}
	if len(scope) != 2 || scope["bankId"] != "comp:auth-service" {
		t.Fatalf("unexpected scope file payload: %#v", scope)
	}
	if !reflect.DeepEqual(scope["tags"], []any{"card:ENG-103", "type:bug"}) {
		t.Fatalf("unexpected scope tags: %#v", scope["tags"])
	}
	if len(commands) != 3 {
		t.Fatalf("dispatch count = %d, want 3", len(commands))
	}
	if commands[0]["type"] != "project.create" || commands[1]["type"] != "thread.create" || commands[2]["type"] != "thread.turn.start" {
		t.Fatalf("unexpected command sequence: %#v", commands)
	}
	message := commands[2]["message"].(map[string]any)
	if message["text"] != "Fix the auth bug" {
		t.Fatalf("turn message = %#v", message)
	}
}
