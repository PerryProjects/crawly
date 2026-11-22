package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/perryprojects/siteinfo/internal/app"
	"github.com/perryprojects/siteinfo/internal/config"
	"github.com/perryprojects/siteinfo/internal/observability"
)

func newServeCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API",
		Long: "Run the HTTP API.\n\nConfiguration comes from an optional YAML file (--config) and SITEINFO_* " +
			"environment variables, which win over the file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			log, err := observability.NewLogger(cfg.Env, cfg.Log.Level, observability.Sampling{Initial: cfg.Log.SamplingInitial, Thereafter: cfg.Log.SamplingThereafter})
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			a, err := app.New(cfg, log)
			if err != nil {
				return fmt.Errorf("start: %w", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.Run(ctx)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", os.Getenv("SITEINFO_CONFIG"), "path to a YAML config file (default $SITEINFO_CONFIG)")
	return cmd
}
