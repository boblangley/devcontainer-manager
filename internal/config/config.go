package config

import (
	"errors"
	"os"
	"regexp"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server        ServerConfig        `yaml:"server" json:"server"`
	Discovery     DiscoveryConfig     `yaml:"discovery" json:"discovery"`
	Defaults      DefaultsConfig      `yaml:"defaults" json:"defaults"`
	T3            T3Config            `yaml:"t3" json:"t3"`
	PairingTokens PairingTokensConfig `yaml:"pairingTokens" json:"pairingTokens"`
	Sync          SyncConfig          `yaml:"sync" json:"sync"`
	Rules         []Rule              `yaml:"rules" json:"rules"`
}

type ServerConfig struct {
	Listen           string `yaml:"listen" json:"listen"`
	AllowNonLoopback bool   `yaml:"allowNonLoopback" json:"allowNonLoopback"`
}

type DiscoveryConfig struct {
	IncludeNamePrefixes []string          `yaml:"includeNamePrefixes" json:"includeNamePrefixes"`
	IncludeLabels       map[string]string `yaml:"includeLabels" json:"includeLabels"`
	ConnectToNetwork    string            `yaml:"connectToNetwork" json:"connectToNetwork"`
}

type DefaultsConfig struct {
	ContainerUser string `yaml:"containerUser" json:"containerUser"`
	ContainerHome string `yaml:"containerHome" json:"containerHome"`
}

type T3Config struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	Port                  int      `yaml:"port" json:"port"`
	AutoIssueBackendToken bool     `yaml:"autoIssueBackendToken" json:"autoIssueBackendToken"`
	ServerCommand         []string `yaml:"serverCommand" json:"serverCommand"`
}

type PairingTokensConfig struct {
	PostgresURL string `yaml:"postgresUrl" json:"postgresUrl"`
}

type SyncConfig struct {
	Enabled  bool         `yaml:"enabled" json:"enabled"`
	Defaults SyncDefaults `yaml:"defaults" json:"defaults"`
}

type SyncDefaults struct {
	IgnoreVCS   bool              `yaml:"ignoreVCS" json:"ignoreVCS"`
	Ignores     []string          `yaml:"ignores" json:"ignores"`
	Permissions PermissionConfig  `yaml:"permissions" json:"permissions"`
	Extra       map[string]string `yaml:",inline" json:"-"`
}

type PermissionConfig struct {
	DefaultFileMode      string `yaml:"defaultFileMode" json:"defaultFileMode"`
	DefaultDirectoryMode string `yaml:"defaultDirectoryMode" json:"defaultDirectoryMode"`
}

type Rule struct {
	Name          string      `yaml:"name" json:"name"`
	Match         MatchRule   `yaml:"match" json:"match"`
	ContainerUser string      `yaml:"containerUser" json:"containerUser,omitempty"`
	ContainerHome string      `yaml:"containerHome" json:"containerHome,omitempty"`
	T3            RuleT3      `yaml:"t3" json:"t3"`
	Syncs         []SyncEntry `yaml:"syncs" json:"syncs"`
}

type RuleT3 struct {
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
}

type MatchRule struct {
	LocalFolder Matcher           `yaml:"localFolder" json:"localFolder"`
	ConfigFile  Matcher           `yaml:"configFile" json:"configFile"`
	Name        Matcher           `yaml:"name" json:"name"`
	Labels      map[string]string `yaml:"labels" json:"labels"`
}

type Matcher struct {
	Literal string `yaml:"literal" json:"literal,omitempty"`
	Glob    string `yaml:"glob" json:"glob,omitempty"`
	Regex   string `yaml:"regex" json:"regex,omitempty"`
}

type SyncEntry struct {
	Mode        string           `yaml:"mode" json:"mode"`
	Manager     string           `yaml:"manager" json:"manager"`
	Container   string           `yaml:"container" json:"container"`
	Replica     bool             `yaml:"replica" json:"replica"`
	IgnoreVCS   *bool            `yaml:"ignoreVCS" json:"ignoreVCS,omitempty"`
	Ignores     []string         `yaml:"ignores" json:"ignores,omitempty"`
	Permissions PermissionConfig `yaml:"permissions" json:"permissions"`
}

func Default() Config {
	return Config{
		Server: ServerConfig{Listen: "127.0.0.1:8787"},
		Discovery: DiscoveryConfig{
			IncludeNamePrefixes: []string{"vsc-"},
			IncludeLabels:       map[string]string{},
		},
		Defaults: DefaultsConfig{ContainerUser: "vscode", ContainerHome: "/home/vscode"},
		T3: T3Config{
			Enabled:               true,
			Port:                  3773,
			AutoIssueBackendToken: true,
			ServerCommand:         []string{"t3", "serve", "--host", "0.0.0.0", "--port", "3773"},
		},
		Sync: SyncConfig{
			Enabled: true,
			Defaults: SyncDefaults{
				IgnoreVCS: true,
				Ignores:   []string{".DS_Store"},
				Permissions: PermissionConfig{
					DefaultFileMode:      "0644",
					DefaultDirectoryMode: "0755",
				},
			},
		},
		Rules: []Rule{{Name: "default"}},
	}
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	cfg.ApplyDefaults()
	return cfg, cfg.Validate()
}

func (c *Config) ApplyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8787"
	}
	if c.Defaults.ContainerUser == "" {
		c.Defaults.ContainerUser = "vscode"
	}
	if c.Defaults.ContainerHome == "" {
		c.Defaults.ContainerHome = "/home/vscode"
	}
	if c.T3.Port == 0 {
		c.T3.Port = 3773
	}
	if len(c.Discovery.IncludeNamePrefixes) == 0 {
		c.Discovery.IncludeNamePrefixes = []string{"vsc-"}
	}
	if c.Discovery.IncludeLabels == nil {
		c.Discovery.IncludeLabels = map[string]string{}
	}
	if c.Sync.Defaults.Permissions.DefaultFileMode == "" {
		c.Sync.Defaults.Permissions.DefaultFileMode = "0644"
	}
	if c.Sync.Defaults.Permissions.DefaultDirectoryMode == "" {
		c.Sync.Defaults.Permissions.DefaultDirectoryMode = "0755"
	}
	if len(c.Rules) == 0 {
		c.Rules = []Rule{{Name: "default"}}
	}
	for index := range c.Rules {
		if c.Rules[index].Name == "" {
			c.Rules[index].Name = "rule"
		}
	}
}

func (c Config) Validate() error {
	for _, rule := range c.Rules {
		if err := rule.Match.LocalFolder.Validate(); err != nil {
			return err
		}
		if err := rule.Match.ConfigFile.Validate(); err != nil {
			return err
		}
		if err := rule.Match.Name.Validate(); err != nil {
			return err
		}
		for _, sync := range rule.Syncs {
			if sync.Manager == "" || sync.Container == "" {
				return errors.New("sync entries require manager and container paths")
			}
			switch sync.Mode {
			case "", "push", "pull", "two-way":
			default:
				return errors.New("sync mode must be push, pull, or two-way")
			}
		}
	}
	return nil
}

func (m Matcher) Validate() error {
	if m.Regex != "" {
		_, err := regexp.Compile(m.Regex)
		return err
	}
	return nil
}

func (m Matcher) Empty() bool {
	return m.Literal == "" && m.Glob == "" && m.Regex == ""
}

func (m Matcher) Match(value string) bool {
	if m.Empty() {
		return true
	}
	if m.Literal != "" && value != m.Literal {
		return false
	}
	if m.Glob != "" {
		ok, err := doublestar.PathMatch(m.Glob, value)
		if err != nil || !ok {
			return false
		}
	}
	if m.Regex != "" {
		ok, err := regexp.MatchString(m.Regex, value)
		if err != nil || !ok {
			return false
		}
	}
	return true
}
