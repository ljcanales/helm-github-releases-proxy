package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"helm-github-releases-proxy/internal/app"
	"helm-github-releases-proxy/internal/config"
	githubclient "helm-github-releases-proxy/internal/github"
	"helm-github-releases-proxy/internal/logging"
)

func main() {
	cfg, configErr := config.LoadWithLookup(config.EnvironmentLookup)
	logger := logging.NewJSON(os.Stdout, cfg.LogLevel)
	events := logging.New(logger)
	if configErr != nil {
		events.Error(context.Background(), "invalid configuration", "error", configErr)
	}

	service := app.New(cfg, configErr, logger, githubclient.NewClient(nil, logger), time.Now, context.Background())
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
			events.Error(shutdownCtx, "server shutdown failed", "error", err)
		}
	}()

	events.Info(context.Background(), "server starting", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		events.Error(context.Background(), "server stopped", "error", err)
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
