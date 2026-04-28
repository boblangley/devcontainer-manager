package config

import "testing"

func TestMatcher(t *testing.T) {
	tests := []struct {
		name    string
		matcher Matcher
		value   string
		want    bool
	}{
		{name: "empty matches", matcher: Matcher{}, value: "anything", want: true},
		{name: "literal matches", matcher: Matcher{Literal: "/repo"}, value: "/repo", want: true},
		{name: "literal misses", matcher: Matcher{Literal: "/repo"}, value: "/other", want: false},
		{name: "glob matches doublestar", matcher: Matcher{Glob: "/Users/me/Code/**"}, value: "/Users/me/Code/work/app", want: true},
		{name: "regex matches", matcher: Matcher{Regex: `^vsc-.+`}, value: "vsc-repo", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.matcher.Match(tt.value); got != tt.want {
				t.Fatalf("Match() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	if cfg.Server.Listen != "127.0.0.1:8787" {
		t.Fatalf("unexpected listen default: %s", cfg.Server.Listen)
	}
	if cfg.Defaults.ContainerUser != "vscode" || cfg.Defaults.ContainerHome != "/home/vscode" {
		t.Fatalf("unexpected container defaults: %#v", cfg.Defaults)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Name != "default" {
		t.Fatalf("unexpected default rules: %#v", cfg.Rules)
	}
}
