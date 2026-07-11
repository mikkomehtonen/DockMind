package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dockmind/dockmind/internal/api"
	"github.com/dockmind/dockmind/internal/config"
	"github.com/dockmind/dockmind/internal/docker"
	"github.com/dockmind/dockmind/internal/gateway"
	"github.com/dockmind/dockmind/internal/gpu"
	"github.com/dockmind/dockmind/internal/health"
	"github.com/dockmind/dockmind/internal/shelly"
	"github.com/dockmind/dockmind/internal/state"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	configPath := flag.String("config", "./config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	power := shelly.New(cfg.Shelly.Address, cfg.Shelly.Channel)
	gpuMonitor := gpu.New(logger)
	dockerClient := docker.New(cfg.Docker.Container)
	healthClient := health.New(cfg.LlamaSwap.HealthURL)

	machine := state.New(
		power,
		gpuMonitor,
		dockerClient,
		healthClient,
		logger,
		cfg.GPU.PollInterval.Duration(),
		cfg.Startup.Timeout.Duration(),
		cfg.Shutdown.Timeout.Duration(),
	)

	server := api.NewServer(machine, logger)

	var gw *gateway.Gateway
	if cfg.Gateway.Enabled {
		gw, err = gateway.NewGatewayWithPollInterval(
			cfg.LlamaSwap.BackendURL,
			cfg.Gateway.IdleTimeout.Duration(),
			cfg.Gateway.RequestTimeout.Duration(),
			cfg.GPU.PollInterval.Duration(),
			machine,
			logger,
		)
		if err != nil {
			logger.Error("failed to create gateway", "error", err)
			os.Exit(1)
		}
		server.SetGatewayHandlers(gw.Handler(), gw.ModelsHandler())
		gw.StartIdleWatcher(context.Background())
	}

	httpServer := &http.Server{
		Addr:    cfg.Server.Address,
		Handler: server.Handler(),
	}

	go func() {
		logger.Info("starting server", "addr", cfg.Server.Address)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down server")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if gw != nil {
		gw.StopIdleWatcher()
	}
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "error", err)
	}

	logger.Info("waiting for any in-flight transitions")
	machine.Wait()
}
