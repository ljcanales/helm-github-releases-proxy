package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/startup"
)

func main() {
	cfg, configErr := config.LoadWithLookup(config.EnvironmentLookup)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if configErr != nil {
		logger.Error("invalid configuration", "error", configErr)
	}

	service := startup.New(cfg, configErr, logger, githubclient.NewClient(nil), time.Now, context.Background())
	service.Start()
	server := &http.Server{
		Addr:    ":" + itoa(cfg.Port),
		Handler: service.Handler,
	}

	stop, stopCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopCancel()
	go func() {
		<-stop.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown failed", "error", err)
		}
	}()

	logger.Info("server starting", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

const shutdownTimeout time.Duration = 5 * time.Second

func itoa(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	result := ""
	for value > 0 {
		result = string(digits[value%10]) + result
		value /= 10
	}
	return result
}
