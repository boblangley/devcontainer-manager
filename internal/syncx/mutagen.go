package syncx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/bob/devcontainer-manager/internal/config"
	"github.com/bob/devcontainer-manager/internal/model"
	"github.com/mutagen-io/mutagen/pkg/daemon"
	"github.com/mutagen-io/mutagen/pkg/filesystem"
	"github.com/mutagen-io/mutagen/pkg/grpcutil"
	"github.com/mutagen-io/mutagen/pkg/ipc"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/prompting"
	"github.com/mutagen-io/mutagen/pkg/selection"
	daemonsvc "github.com/mutagen-io/mutagen/pkg/service/daemon"
	promptingsvc "github.com/mutagen-io/mutagen/pkg/service/prompting"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core/ignore"
	mutagenurl "github.com/mutagen-io/mutagen/pkg/url"
)

const daemonDialTimeout = 750 * time.Millisecond

type mutagenClient struct {
	logger *slog.Logger
	mu     sync.Mutex
	conn   *grpc.ClientConn
	sync   synchronizationsvc.SynchronizationClient
	prompt promptingsvc.PromptingClient
}

type logPrompter struct {
	logger *slog.Logger
}

var _ prompting.Prompter = (*logPrompter)(nil)

func newMutagenClient(logger *slog.Logger) *mutagenClient {
	return &mutagenClient{logger: logger}
}

func (c *mutagenClient) ensureConnected(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return nil
	}
	endpoint, err := daemon.EndpointPath()
	if err != nil {
		return fmt.Errorf("unable to compute daemon endpoint path: %w", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, daemonDialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(
		dialCtx,
		endpoint,
		grpc.WithInsecure(),
		grpc.WithContextDialer(ipc.DialContext),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(grpcutil.MaximumMessageSize),
			grpc.MaxCallRecvMsgSize(grpcutil.MaximumMessageSize),
		),
	)
	if err != nil {
		return err
	}
	if err := verifyDaemonVersion(ctx, conn); err != nil {
		_ = conn.Close()
		return err
	}
	c.conn = conn
	c.sync = synchronizationsvc.NewSynchronizationClient(conn)
	c.prompt = promptingsvc.NewPromptingClient(conn)
	return nil
}

func (c *mutagenClient) listOwned(ctx context.Context) (map[string]liveSession, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	response, err := c.sync.List(ctx, &synchronizationsvc.ListRequest{
		Selection: &selection.Selection{LabelSelector: labelOwned + "=true"},
	})
	if err != nil {
		c.reset()
		return nil, grpcutil.PeelAwayRPCErrorLayer(err)
	}
	if err := response.EnsureValid(); err != nil {
		return nil, err
	}
	live := map[string]liveSession{}
	for _, state := range response.SessionStates {
		if state.Session == nil {
			continue
		}
		session := liveSession{
			ID:     state.Session.Identifier,
			Name:   state.Session.Name,
			Status: mutagenStatus(state),
		}
		if session.Name == "" {
			session.Name = session.ID
		}
		live[session.Name] = session
	}
	return live, nil
}

func (c *mutagenClient) create(ctx context.Context, session model.Session, syncEntry config.SyncEntry, defaults config.SyncDefaults) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	specification, err := creationSpecification(session, syncEntry, defaults)
	if err != nil {
		return err
	}
	prompter, stopPrompting, err := c.hostPrompter(ctx, true)
	if err != nil {
		return err
	}
	defer stopPrompting()
	response, err := c.sync.Create(ctx, &synchronizationsvc.CreateRequest{
		Prompter:      prompter,
		Specification: specification,
	})
	if err != nil {
		c.reset()
		return grpcutil.PeelAwayRPCErrorLayer(err)
	}
	return response.EnsureValid()
}

func (c *mutagenClient) pause(ctx context.Context, specification string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	prompter, stopPrompting, err := c.hostPrompter(ctx, false)
	if err != nil {
		return err
	}
	defer stopPrompting()
	response, err := c.sync.Pause(ctx, &synchronizationsvc.PauseRequest{
		Prompter:  prompter,
		Selection: sessionSelection(specification),
	})
	if err != nil {
		c.reset()
		return grpcutil.PeelAwayRPCErrorLayer(err)
	}
	return response.EnsureValid()
}

func (c *mutagenClient) resume(ctx context.Context, specification string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	prompter, stopPrompting, err := c.hostPrompter(ctx, true)
	if err != nil {
		return err
	}
	defer stopPrompting()
	response, err := c.sync.Resume(ctx, &synchronizationsvc.ResumeRequest{
		Prompter:  prompter,
		Selection: sessionSelection(specification),
	})
	if err != nil {
		c.reset()
		return grpcutil.PeelAwayRPCErrorLayer(err)
	}
	return response.EnsureValid()
}

func (c *mutagenClient) terminate(ctx context.Context, specification string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	prompter, stopPrompting, err := c.hostPrompter(ctx, false)
	if err != nil {
		return err
	}
	defer stopPrompting()
	response, err := c.sync.Terminate(ctx, &synchronizationsvc.TerminateRequest{
		Prompter:  prompter,
		Selection: sessionSelection(specification),
	})
	if err != nil {
		c.reset()
		return grpcutil.PeelAwayRPCErrorLayer(err)
	}
	return response.EnsureValid()
}

func (c *mutagenClient) hostPrompter(ctx context.Context, allowPrompts bool) (string, func(), error) {
	promptCtx, cancel := context.WithCancel(ctx)
	prompter, promptErrors, err := promptingsvc.Host(promptCtx, c.prompt, &logPrompter{logger: c.logger}, allowPrompts)
	if err != nil {
		cancel()
		return "", nil, fmt.Errorf("unable to host mutagen prompter: %w", err)
	}
	stop := func() {
		cancel()
		<-promptErrors
	}
	return prompter, stop, nil
}

func (c *mutagenClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.sync = nil
	c.prompt = nil
}

func verifyDaemonVersion(ctx context.Context, conn *grpc.ClientConn) error {
	response, err := daemonsvc.NewDaemonClient(conn).Version(ctx, &daemonsvc.VersionRequest{})
	if err != nil {
		return fmt.Errorf("unable to query daemon version: %w", grpcutil.PeelAwayRPCErrorLayer(err))
	}
	if response.Major != mutagen.VersionMajor ||
		response.Minor != mutagen.VersionMinor ||
		response.Patch != mutagen.VersionPatch ||
		response.Tag != mutagen.VersionTag {
		return fmt.Errorf("client/daemon version mismatch: client=%d.%d.%d%s daemon=%d.%d.%d%s",
			mutagen.VersionMajor, mutagen.VersionMinor, mutagen.VersionPatch, mutagen.VersionTag,
			response.Major, response.Minor, response.Patch, response.Tag,
		)
	}
	return nil
}

func creationSpecification(session model.Session, syncEntry config.SyncEntry, defaults config.SyncDefaults) (*synchronizationsvc.CreationSpecification, error) {
	alpha, err := mutagenurl.Parse(session.Alpha, mutagenurl.Kind_Synchronization, true)
	if err != nil {
		return nil, fmt.Errorf("unable to parse alpha URL: %w", err)
	}
	beta, err := mutagenurl.Parse(session.Beta, mutagenurl.Kind_Synchronization, false)
	if err != nil {
		return nil, fmt.Errorf("unable to parse beta URL: %w", err)
	}
	mode, err := synchronizationMode(session.Mode, syncEntry.Replica)
	if err != nil {
		return nil, err
	}
	ignoreVCS := defaults.IgnoreVCS
	if syncEntry.IgnoreVCS != nil {
		ignoreVCS = *syncEntry.IgnoreVCS
	}
	ignoreVCSMode := ignore.IgnoreVCSMode_IgnoreVCSModePropagate
	if ignoreVCS {
		ignoreVCSMode = ignore.IgnoreVCSMode_IgnoreVCSModeIgnore
	}
	permissions := defaults.Permissions
	if syncEntry.Permissions.DefaultFileMode != "" {
		permissions.DefaultFileMode = syncEntry.Permissions.DefaultFileMode
	}
	if syncEntry.Permissions.DefaultDirectoryMode != "" {
		permissions.DefaultDirectoryMode = syncEntry.Permissions.DefaultDirectoryMode
	}
	defaultFileMode, err := parseFilesystemMode(permissions.DefaultFileMode)
	if err != nil {
		return nil, fmt.Errorf("invalid default file mode: %w", err)
	}
	defaultDirectoryMode, err := parseFilesystemMode(permissions.DefaultDirectoryMode)
	if err != nil {
		return nil, fmt.Errorf("invalid default directory mode: %w", err)
	}
	configuration := &synchronization.Configuration{
		SynchronizationMode:  mode,
		Ignores:              append(append([]string{}, defaults.Ignores...), syncEntry.Ignores...),
		IgnoreVCSMode:        ignoreVCSMode,
		DefaultFileMode:      uint32(defaultFileMode),
		DefaultDirectoryMode: uint32(defaultDirectoryMode),
	}
	if err := configuration.EnsureValid(false); err != nil {
		return nil, fmt.Errorf("invalid synchronization configuration: %w", err)
	}
	specification := &synchronizationsvc.CreationSpecification{
		Alpha:              alpha,
		Beta:               beta,
		Configuration:      configuration,
		ConfigurationAlpha: &synchronization.Configuration{},
		ConfigurationBeta:  &synchronization.Configuration{},
		Name:               session.Name,
		Labels:             session.Labels,
	}
	return specification, nil
}

func synchronizationMode(mode string, replica bool) (core.SynchronizationMode, error) {
	var parsed core.SynchronizationMode
	if err := parsed.UnmarshalText([]byte(mutagenMode(mode, replica))); err != nil {
		return 0, err
	}
	return parsed, nil
}

func parseFilesystemMode(value string) (filesystem.Mode, error) {
	if value == "" {
		return 0, nil
	}
	var mode filesystem.Mode
	if err := mode.UnmarshalText([]byte(value)); err != nil {
		return 0, err
	}
	return mode, nil
}

func mutagenStatus(state *synchronization.State) string {
	if state == nil || state.Session == nil {
		return "unknown"
	}
	if state.Session.Paused {
		return "paused"
	}
	if state.LastError != "" {
		return "error"
	}
	switch state.Status {
	case synchronization.Status_Watching:
		return "ready"
	case synchronization.Status_Disconnected:
		return "disconnected"
	default:
		value := strings.TrimPrefix(state.Status.String(), "Status_")
		value = strings.ReplaceAll(value, "_", "-")
		return strings.ToLower(value)
	}
}

func (p *logPrompter) Message(message string) error {
	p.logger.Info("mutagen", "message", message)
	return nil
}

func (p *logPrompter) Prompt(message string) (string, error) {
	p.logger.Warn("mutagen prompt rejected", "message", message)
	return "", errors.New("interactive Mutagen prompts are not supported")
}
