package syncx

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/bob/devcontainer-manager/internal/config"
	"github.com/bob/devcontainer-manager/internal/dockerx"
	"github.com/bob/devcontainer-manager/internal/model"
	"github.com/mutagen-io/mutagen/pkg/selection"
)

const (
	labelOwned     = "devcontainer-manager/owned"
	labelContainer = "devcontainer-manager/container-id"
	labelRule      = "devcontainer-manager/rule"
	labelSyncIndex = "devcontainer-manager/sync-index"
)

type Controller struct {
	docker *dockerx.Client
	client *mutagenClient
	logger *slog.Logger
}

type liveSession struct {
	ID     string
	Name   string
	Status string
}

func New(docker *dockerx.Client, logger *slog.Logger) *Controller {
	return &Controller{docker: docker, client: newMutagenClient(logger), logger: logger}
}

func (c *Controller) Reconcile(ctx context.Context, containers []model.Container, cfg config.Config) []model.Session {
	var sessions []model.Session
	if !cfg.Sync.Enabled {
		return sessions
	}
	live := map[string]liveSession{}
	mutagenAvailable := true
	if err := c.client.ensureConnected(ctx); err != nil {
		mutagenAvailable = false
		c.logger.Warn("failed to connect to mutagen daemon", "error", err)
	} else if listed, err := c.client.listOwned(ctx); err != nil {
		mutagenAvailable = false
		c.logger.Warn("failed to list mutagen sessions", "error", err)
	} else {
		live = listed
	}
	desiredNames := map[string]bool{}
	for _, container := range containers {
		if container.Rule == nil || len(container.Rule.Syncs) == 0 {
			continue
		}
		for index, sync := range container.Rule.Syncs {
			session := c.desiredSession(container, container.Rule.Name, index, sync)
			desiredNames[session.Name] = true
			if !container.Running {
				session.Status = "paused"
				sessions = append(sessions, session)
				if mutagenAvailable {
					_ = c.client.pause(ctx, session.Name)
				}
				continue
			}
			if err := c.validateEndpoints(ctx, container.ID, sync); err != nil {
				session.Status = "configuration-error"
				session.Error = err.Error()
				sessions = append(sessions, session)
				continue
			}
			if !mutagenAvailable {
				session.Status = "mutagen-daemon-unavailable"
				session.Error = "unable to connect to Mutagen daemon"
				sessions = append(sessions, session)
				continue
			}
			if existing, ok := live[session.Name]; ok {
				session.ID = firstNonEmpty(existing.ID, session.ID)
				session.Status = firstNonEmpty(existing.Status, "ready")
				sessions = append(sessions, session)
				_ = c.client.resume(ctx, session.Name)
				continue
			}
			if err := c.client.create(ctx, session, sync, cfg.Sync.Defaults); err != nil {
				session.Status = "create-error"
				session.Error = err.Error()
			} else {
				session.Status = "ready"
			}
			sessions = append(sessions, session)
		}
	}
	if mutagenAvailable {
		for name, existing := range live {
			if !desiredNames[name] {
				target := firstNonEmpty(existing.ID, existing.Name)
				if target != "" {
					_ = c.client.terminate(ctx, target)
				}
			}
		}
	}
	return sessions
}

func (c *Controller) desiredSession(container model.Container, rule string, index int, sync config.SyncEntry) model.Session {
	mode := firstNonEmpty(sync.Mode, "two-way")
	name := sessionName(container.ID, rule, index, sync)
	containerURL := "docker://" + container.ContainerUser + "@" + container.ID + sync.Container
	alpha, beta := sync.Manager, containerURL
	if mode == "pull" {
		alpha, beta = containerURL, sync.Manager
	}
	labels := map[string]string{
		labelOwned:     "true",
		labelContainer: container.ID,
		labelRule:      rule,
		labelSyncIndex: strconv.Itoa(index),
	}
	return model.Session{
		ID:          name,
		Name:        name,
		ContainerID: container.ID,
		Rule:        rule,
		SyncIndex:   index,
		Mode:        mode,
		Alpha:       alpha,
		Beta:        beta,
		Status:      "desired",
		Labels:      labels,
		Desired:     true,
	}
}

func (c *Controller) validateEndpoints(ctx context.Context, containerID string, sync config.SyncEntry) error {
	managerKind := localKind(sync.Manager)
	containerKind, err := c.docker.PathKind(ctx, containerID, sync.Container)
	if err != nil {
		return err
	}
	if managerKind == "missing" && containerKind == "missing" {
		return fmt.Errorf("both endpoints are missing: %s and %s", sync.Manager, sync.Container)
	}
	if managerKind != "missing" && containerKind != "missing" && managerKind != containerKind {
		return fmt.Errorf("endpoint type mismatch: manager is %s, container is %s", managerKind, containerKind)
	}
	return nil
}

func mutagenMode(mode string, replica bool) string {
	switch mode {
	case "push", "pull":
		if replica {
			return "one-way-replica"
		}
		return "one-way-safe"
	case "two-way":
		if replica {
			return "two-way-resolved"
		}
	}
	return "two-way-safe"
}

func sessionSelection(specification string) *selection.Selection {
	return &selection.Selection{Specifications: []string{specification}}
}

func sessionName(containerID, rule string, index int, sync config.SyncEntry) string {
	sum := sha1.Sum([]byte(containerID + "|" + rule + "|" + strconv.Itoa(index) + "|" + sync.Manager + "|" + sync.Container))
	return "dcm-" + shortID(containerID) + "-" + strconv.Itoa(index) + "-" + hex.EncodeToString(sum[:])[:8]
}

func localKind(path string) string {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "other"
	}
	if info.IsDir() {
		return "directory"
	}
	if info.Mode().IsRegular() {
		return "file"
	}
	return "other"
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
