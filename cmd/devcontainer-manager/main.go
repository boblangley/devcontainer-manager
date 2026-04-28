package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bob/devcontainer-manager/internal/app"
	"github.com/bob/devcontainer-manager/internal/config"
)

func main() {
	var configPath string
	var listenOverride string
	var allowNonLoopback bool
	flag.StringVar(&configPath, "config", envOrDefault("DCM_CONFIG", "/etc/devcontainer-manager/config.yaml"), "path to config.yaml")
	flag.StringVar(&listenOverride, "listen", os.Getenv("DCM_LISTEN"), "HTTP listen address override")
	flag.BoolVar(&allowNonLoopback, "allow-non-loopback", envBool("DCM_ALLOW_NON_LOOPBACK"), "allow binding the HTTP server to a non-loopback address")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(1)
	}
	if errors.Is(err, os.ErrNotExist) {
		logger.Warn("config file not found, using defaults", "path", configPath)
		cfg = config.Default()
	}
	if listenOverride != "" {
		cfg.Server.Listen = listenOverride
	}
	if allowNonLoopback {
		cfg.Server.AllowNonLoopback = true
	}
	if err := validateListen(cfg.Server.Listen, cfg.Server.AllowNonLoopback); err != nil {
		logger.Error("refusing listen address", "listen", cfg.Server.Listen, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	service, err := app.New(configPath, cfg, logger)
	if err != nil {
		logger.Error("failed to initialize service", "error", err)
		os.Exit(1)
	}
	if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("service stopped with error", "error", err)
		os.Exit(1)
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes"
}

func validateListen(addr string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil {
			return err
		}
		for _, resolved := range ips {
			if !resolved.IsLoopback() && !allowNonLoopback {
				return errors.New("non-loopback bindings require --allow-non-loopback")
			}
		}
		return nil
	}
	if !ip.IsLoopback() && !allowNonLoopback {
		return errors.New("non-loopback bindings require --allow-non-loopback")
	}
	return nil
}
