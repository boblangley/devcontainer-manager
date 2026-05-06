//go:build integration

package dockerx

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	containerTypes "github.com/docker/docker/api/types/container"
)

func TestExecWithStdinWritesRetainScopeAsContainerUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer client.Close()

	const image = "alpine:3.20"
	pull, err := client.api.ImagePull(ctx, image, types.ImagePullOptions{})
	if err != nil {
		t.Skipf("pull %s: %v", image, err)
	}
	_, _ = io.Copy(io.Discard, pull)
	_ = pull.Close()

	created, err := client.api.ContainerCreate(ctx, &containerTypes.Config{
		Image: image,
		Cmd:   []string{"sleep", "120"},
	}, nil, nil, nil, "dcm-exec-stdin-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.api.ContainerRemove(removeCtx, created.ID, containerTypes.RemoveOptions{Force: true})
	}()
	if err := client.api.ContainerStart(ctx, created.ID, containerTypes.StartOptions{}); err != nil {
		t.Fatalf("start container: %v", err)
	}

	target := "/tmp/dcm-workspace/.hindsight/active-retain-scope.json"
	input := []byte(`{"bankId":"comp:auth-service","tags":["card:ENG-103","type:bug"]}` + "\n")
	cmd := []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1.tmp" && mv "$1.tmp" "$1"`, "--", target}
	if err := client.ExecWithStdin(ctx, created.ID, cmd, "nobody", input); err != nil {
		t.Fatalf("ExecWithStdin: %v", err)
	}

	out, err := client.Exec(ctx, created.ID, []string{"sh", "-lc", `cat "$0"; stat -c '%U:%G %a' "$0"`}, target)
	if err != nil {
		t.Fatalf("inspect written file: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected inspect output: %q", out)
	}
	if lines[0] != strings.TrimSpace(string(input)) {
		t.Fatalf("file content = %q, want %q", lines[0], strings.TrimSpace(string(input)))
	}
	if lines[1] != "nobody:nobody 644" {
		t.Fatalf("file owner/mode = %q, want nobody:nobody 644", lines[1])
	}
}
