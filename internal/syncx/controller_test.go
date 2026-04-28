package syncx

import (
	"strings"
	"testing"

	"github.com/bob/devcontainer-manager/internal/config"
	"github.com/bob/devcontainer-manager/internal/model"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
)

func TestMutagenMode(t *testing.T) {
	tests := []struct {
		mode    string
		replica bool
		want    string
	}{
		{mode: "two-way", want: "two-way-safe"},
		{mode: "two-way", replica: true, want: "two-way-resolved"},
		{mode: "push", want: "one-way-safe"},
		{mode: "push", replica: true, want: "one-way-replica"},
		{mode: "pull", want: "one-way-safe"},
		{mode: "pull", replica: true, want: "one-way-replica"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			if got := mutagenMode(tt.mode, tt.replica); got != tt.want {
				t.Fatalf("mutagenMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionNameIsStableAndPrefixed(t *testing.T) {
	sync := config.SyncEntry{Manager: "/sync/gitconfig", Container: "/home/vscode/.gitconfig"}
	first := sessionName("abcdef1234567890", "default", 0, sync)
	second := sessionName("abcdef1234567890", "default", 0, sync)
	if first != second {
		t.Fatalf("session name is not stable: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "dcm-abcdef123456-0-") {
		t.Fatalf("unexpected session prefix: %q", first)
	}
}

func TestCreationSpecification(t *testing.T) {
	session := model.Session{
		Name:   "dcm-test",
		Mode:   "push",
		Alpha:  "/sync/gitconfig",
		Beta:   "docker://vscode@abcdef123456/home/vscode/.gitconfig",
		Labels: map[string]string{labelOwned: "true"},
	}
	specification, err := creationSpecification(session, config.SyncEntry{}, config.SyncDefaults{
		IgnoreVCS: true,
		Ignores:   []string{".DS_Store"},
		Permissions: config.PermissionConfig{
			DefaultFileMode:      "0644",
			DefaultDirectoryMode: "0755",
		},
	})
	if err != nil {
		t.Fatalf("creationSpecification returned error: %v", err)
	}
	if specification.Name != session.Name {
		t.Fatalf("name = %q, want %q", specification.Name, session.Name)
	}
	if specification.Configuration.SynchronizationMode != core.SynchronizationMode_SynchronizationModeOneWaySafe {
		t.Fatalf("mode = %s", specification.Configuration.SynchronizationMode)
	}
	if specification.Configuration.IgnoreVCSMode != ignore.IgnoreVCSMode_IgnoreVCSModeIgnore {
		t.Fatalf("ignore VCS mode = %s", specification.Configuration.IgnoreVCSMode)
	}
	if specification.Configuration.DefaultFileMode != 0644 || specification.Configuration.DefaultDirectoryMode != 0755 {
		t.Fatalf("unexpected default modes: %#v", specification.Configuration)
	}
}
