package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bob/devcontainer-manager/internal/config"
	"github.com/bob/devcontainer-manager/internal/model"
	"github.com/docker/docker/api/types"
	containerTypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	networkTypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type Client struct {
	api    *client.Client
	logger *slog.Logger
}

func New(logger *slog.Logger) (*Client, error) {
	api, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{api: api, logger: logger}, nil
}

func (c *Client) Close() error {
	return c.api.Close()
}

func (c *Client) ListDevcontainers(ctx context.Context, cfg config.Config) ([]model.Container, error) {
	list, err := c.api.ContainerList(ctx, containerTypes.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]model.Container, 0, len(list))
	networkName := strings.TrimSpace(cfg.Discovery.ConnectToNetwork)
	for _, item := range list {
		inspect, err := c.api.ContainerInspect(ctx, item.ID)
		if err != nil {
			c.logger.Warn("failed to inspect container", "container", item.ID, "error", err)
			continue
		}
		record, ok := c.recordFromInspect(ctx, inspect, item, cfg)
		if ok {
			if networkName != "" && record.Running {
				refreshed, connected, err := c.ensureConnectedToNetwork(ctx, inspect, networkName)
				if err != nil {
					c.logger.Warn("failed to connect devcontainer to docker network", "container", record.ID, "network", networkName, "error", err)
					record.Warnings = append(record.Warnings, "failed to connect to docker network "+networkName+": "+err.Error())
				} else {
					if connected {
						c.logger.Info("connected devcontainer to docker network", "container", record.ID, "network", networkName)
					}
					record.ContainerIP = containerIP(refreshed, networkName)
				}
			}
			out = append(out, record)
		}
	}
	return out, nil
}

func (c *Client) recordFromInspect(ctx context.Context, inspect types.ContainerJSON, item types.Container, cfg config.Config) (model.Container, bool) {
	labels := inspect.Config.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	name := strings.TrimPrefix(inspect.Name, "/")
	reasons := discoveryReasons(name, labels, cfg.Discovery)
	if len(reasons) == 0 {
		return model.Container{}, false
	}
	running := inspect.State != nil && inspect.State.Running
	record := model.Container{
		ID:               inspect.ID,
		ShortID:          shortID(inspect.ID),
		Name:             name,
		Image:            inspect.Config.Image,
		Labels:           labels,
		Status:           item.Status,
		Running:          running,
		LocalFolder:      labels["devcontainer.local_folder"],
		ConfigFile:       labels["devcontainer.config_file"],
		WorkspaceFolder:  workspaceFolder(labels),
		Metadata:         parseMetadata(labels["devcontainer.metadata"]),
		ComposeService:   labels["com.docker.compose.service"],
		LastSeen:         time.Now().UTC(),
		ContainerIP:      containerIP(inspect, strings.TrimSpace(cfg.Discovery.ConnectToNetwork)),
		DiscoveryReasons: reasons,
	}
	rule := MatchRule(record, cfg.Rules)
	record.Rule = rule
	if rule != nil {
		record.RuleName = rule.Name
	}
	record.ContainerUser, record.ContainerHome, record.UserSource = c.ResolveUser(ctx, inspect.ID, inspect.Config.User, record.Metadata, rule, cfg.Defaults, running)
	if record.ContainerUser == "root" && (rule == nil || rule.ContainerUser == "") {
		record.Warnings = append(record.Warnings, "resolved container user is root")
	}
	return record, true
}

func (c *Client) ensureConnectedToNetwork(ctx context.Context, inspect types.ContainerJSON, networkName string) (types.ContainerJSON, bool, error) {
	networkName = strings.TrimSpace(networkName)
	if networkName == "" || inspect.ID == "" {
		return inspect, false, nil
	}
	resource, err := c.api.NetworkInspect(ctx, networkName, types.NetworkInspectOptions{})
	if err != nil {
		return inspect, false, err
	}
	if containerConnectedToNetwork(inspect, networkName, resource.ID) {
		return inspect, false, nil
	}
	if err := c.api.NetworkConnect(ctx, resource.ID, inspect.ID, &networkTypes.EndpointSettings{}); err != nil {
		refreshed, inspectErr := c.api.ContainerInspect(ctx, inspect.ID)
		if inspectErr == nil && containerConnectedToNetwork(refreshed, networkName, resource.ID) {
			return refreshed, true, nil
		}
		return inspect, false, err
	}
	refreshed, err := c.api.ContainerInspect(ctx, inspect.ID)
	if err != nil {
		c.logger.Warn("connected devcontainer to docker network but failed to refresh inspect", "container", inspect.ID, "network", networkName, "error", err)
		return inspect, true, nil
	}
	return refreshed, true, nil
}

func containerConnectedToNetwork(inspect types.ContainerJSON, networkName, networkID string) bool {
	return networkEndpoint(inspect, networkName, networkID) != nil
}

func discoveryReasons(name string, labels map[string]string, cfg config.DiscoveryConfig) []string {
	var reasons []string
	if labels["devcontainer.local_folder"] != "" {
		reasons = append(reasons, "devcontainer.local_folder")
	}
	for key := range labels {
		if strings.HasPrefix(key, "devcontainer.") && key != "devcontainer.local_folder" {
			reasons = append(reasons, key)
			break
		}
	}
	for key, value := range cfg.IncludeLabels {
		if labels[key] == value {
			reasons = append(reasons, "configured-label:"+key)
		}
	}
	for _, prefix := range cfg.IncludeNamePrefixes {
		if strings.HasPrefix(name, prefix) {
			reasons = append(reasons, "name-prefix:"+prefix)
		}
	}
	return reasons
}

func MatchRule(container model.Container, rules []config.Rule) *config.Rule {
	for index := range rules {
		rule := &rules[index]
		if !rule.Match.LocalFolder.Match(container.LocalFolder) {
			continue
		}
		if !rule.Match.ConfigFile.Match(container.ConfigFile) {
			continue
		}
		if !rule.Match.Name.Match(container.Name) {
			continue
		}
		matchesLabels := true
		for key, value := range rule.Match.Labels {
			if container.Labels[key] != value {
				matchesLabels = false
				break
			}
		}
		if matchesLabels {
			return rule
		}
	}
	return nil
}

func (c *Client) ResolveUser(ctx context.Context, containerID, configUser string, metadata []map[string]any, rule *config.Rule, defaults config.DefaultsConfig, running bool) (string, string, string) {
	if rule != nil && (rule.ContainerUser != "" || rule.ContainerHome != "") {
		user := firstNonEmpty(rule.ContainerUser, defaults.ContainerUser, "vscode")
		home := firstNonEmpty(rule.ContainerHome, lookupHome(c, ctx, containerID, user, running), defaults.ContainerHome, "/home/"+user)
		return user, home, "rule"
	}
	if user := metadataString(metadata, "remoteUser"); user != "" {
		home := firstNonEmpty(lookupHome(c, ctx, containerID, user, running), defaults.ContainerHome, "/home/"+user)
		return user, home, "metadata.remoteUser"
	}
	if user := metadataString(metadata, "containerUser"); user != "" {
		home := firstNonEmpty(lookupHome(c, ctx, containerID, user, running), defaults.ContainerHome, "/home/"+user)
		return user, home, "metadata.containerUser"
	}
	if configUser != "" {
		home := firstNonEmpty(lookupHome(c, ctx, containerID, configUser, running), defaults.ContainerHome, "/home/"+configUser)
		return configUser, home, "container.Config.User"
	}
	return firstNonEmpty(defaults.ContainerUser, "vscode"), firstNonEmpty(defaults.ContainerHome, "/home/vscode"), "default"
}

func lookupHome(c *Client, ctx context.Context, containerID, user string, running bool) string {
	if !running || user == "" {
		return ""
	}
	out, err := c.Exec(ctx, containerID, []string{"sh", "-lc", "getent passwd \"$0\" | cut -d: -f6"}, user)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (c *Client) Exec(ctx context.Context, containerID string, cmd []string, args ...string) (string, error) {
	full := append([]string{}, cmd...)
	full = append(full, args...)
	resp, err := c.api.ContainerExecCreate(ctx, containerID, types.ExecConfig{
		Cmd:          full,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", err
	}
	attach, err := c.api.ContainerExecAttach(ctx, resp.ID, types.ExecStartCheck{})
	if err != nil {
		return "", err
	}
	defer attach.Close()
	var buf bytes.Buffer
	_, _ = stdcopy.StdCopy(&buf, &buf, attach.Reader)
	inspect, err := c.api.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return buf.String(), err
	}
	if inspect.ExitCode != 0 {
		return buf.String(), errors.New(strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

func (c *Client) ExecWithStdin(ctx context.Context, containerID string, cmd []string, user string, input []byte) error {
	resp, err := c.api.ContainerExecCreate(ctx, containerID, types.ExecConfig{
		Cmd:          append([]string{}, cmd...),
		User:         user,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return err
	}
	attach, err := c.api.ContainerExecAttach(ctx, resp.ID, types.ExecStartCheck{})
	if err != nil {
		return err
	}
	defer attach.Close()
	if len(input) > 0 {
		if _, err := attach.Conn.Write(input); err != nil {
			return err
		}
	}
	if err := attach.CloseWrite(); err != nil {
		return err
	}
	_, _ = stdcopy.StdCopy(io.Discard, io.Discard, attach.Reader)
	inspect, err := c.api.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return errors.New("docker exec exited with code " + strconv.Itoa(inspect.ExitCode))
	}
	return nil
}

func (c *Client) PathKind(ctx context.Context, containerID, path string) (string, error) {
	out, err := c.Exec(ctx, containerID, []string{"sh", "-lc", `p=$0; if [ -d "$p" ]; then echo directory; elif [ -f "$p" ]; then echo file; elif [ -e "$p" ]; then echo other; else echo missing; fi`}, path)
	return strings.TrimSpace(out), err
}

func (c *Client) Events(ctx context.Context) (<-chan events.Message, <-chan error) {
	args := filters.NewArgs()
	args.Add("type", "container")
	for _, action := range []string{"start", "die", "destroy", "restart"} {
		args.Add("event", action)
	}
	return c.api.Events(ctx, types.EventsOptions{Filters: args})
}

func parseMetadata(raw string) []map[string]any {
	if raw == "" {
		return nil
	}
	var array []map[string]any
	if err := json.Unmarshal([]byte(raw), &array); err == nil {
		return array
	}
	var single map[string]any
	if err := json.Unmarshal([]byte(raw), &single); err == nil {
		return []map[string]any{single}
	}
	return nil
}

func metadataString(metadata []map[string]any, key string) string {
	for i := len(metadata) - 1; i >= 0; i-- {
		if value, ok := metadata[i][key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func workspaceFolder(labels map[string]string) string {
	if folder := labels["devcontainer.workspace_folder"]; folder != "" {
		return folder
	}
	return labels["devcontainer.local_folder"]
}

func containerIP(inspect types.ContainerJSON, preferredNetwork string) string {
	if inspect.NetworkSettings == nil {
		return ""
	}
	if preferredNetwork != "" {
		if network := networkEndpoint(inspect, preferredNetwork, preferredNetwork); network != nil && network.IPAddress != "" {
			return network.IPAddress
		}
	}
	for _, network := range inspect.NetworkSettings.Networks {
		if network.IPAddress != "" {
			return network.IPAddress
		}
	}
	return inspect.NetworkSettings.IPAddress
}

func networkEndpoint(inspect types.ContainerJSON, networkName, networkID string) *networkTypes.EndpointSettings {
	if inspect.NetworkSettings == nil {
		return nil
	}
	if networkName != "" {
		if endpoint := inspect.NetworkSettings.Networks[networkName]; endpoint != nil {
			return endpoint
		}
	}
	if networkID != "" {
		for _, endpoint := range inspect.NetworkSettings.Networks {
			if endpoint != nil && endpoint.NetworkID == networkID {
				return endpoint
			}
		}
	}
	return nil
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

func TarEmptyDir(path string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(path, "/"), Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
