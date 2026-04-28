package model

import (
	"time"

	"github.com/bob/devcontainer-manager/internal/config"
)

type Container struct {
	ID               string            `json:"id"`
	ShortID          string            `json:"shortId"`
	Name             string            `json:"name"`
	Image            string            `json:"image"`
	Labels           map[string]string `json:"labels"`
	Status           string            `json:"status"`
	Running          bool              `json:"running"`
	LocalFolder      string            `json:"localFolder"`
	ConfigFile       string            `json:"configFile"`
	WorkspaceFolder  string            `json:"workspace"`
	Metadata         []map[string]any  `json:"metadata"`
	ComposeService   string            `json:"composeService,omitempty"`
	ContainerUser    string            `json:"containerUser"`
	ContainerHome    string            `json:"containerHome"`
	UserSource       string            `json:"userSource"`
	RuleName         string            `json:"rule"`
	Rule             *config.Rule      `json:"-"`
	LastSeen         time.Time         `json:"lastSeen"`
	ContainerIP      string            `json:"containerIp,omitempty"`
	T3               T3Status          `json:"t3"`
	Sync             SyncSummary       `json:"sync"`
	DiscoveryReasons []string          `json:"discoveryReasons"`
	Warnings         []string          `json:"warnings,omitempty"`
}

type T3Status struct {
	Enabled       bool      `json:"enabled"`
	EnvironmentID string    `json:"environmentId,omitempty"`
	HTTPBaseURL   string    `json:"httpBaseUrl,omitempty"`
	WSBaseURL     string    `json:"wsBaseUrl,omitempty"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	BackendURL    string    `json:"-"`
	BackendToken  string    `json:"-"`
	LastProbe     time.Time `json:"lastProbe,omitempty"`
}

type SyncSummary struct {
	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"`
	Sessions int    `json:"sessions"`
	Error    string `json:"error,omitempty"`
}

type Environment struct {
	ID          string    `json:"id"`
	ContainerID string    `json:"containerId"`
	Name        string    `json:"name"`
	HTTPBaseURL string    `json:"httpBaseUrl"`
	WSBaseURL   string    `json:"wsBaseUrl"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	LastProbe   time.Time `json:"lastProbe,omitempty"`
}

type Session struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	ContainerID string            `json:"containerId"`
	Rule        string            `json:"rule"`
	SyncIndex   int               `json:"syncIndex"`
	Mode        string            `json:"mode"`
	Alpha       string            `json:"alpha"`
	Beta        string            `json:"beta"`
	Status      string            `json:"status"`
	Error       string            `json:"error,omitempty"`
	Labels      map[string]string `json:"labels"`
	Desired     bool              `json:"desired"`
}
