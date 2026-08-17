package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jmon/internal/daemon"

	"github.com/spf13/cobra"
)

// stopCmd stops the running daemon
var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return daemon.Stop()
	},
}

// restartCmd stops the old daemon and starts a new one
var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Stop old daemon, ignore error if not running
		daemon.Stop()
		time.Sleep(500 * time.Millisecond)

		// Start new daemon
		port, _ := cmd.Flags().GetInt("port")
		interval, _ := cmd.Flags().GetInt("interval")
		cfg := &daemon.Config{
			Port:     port,
			Interval: time.Duration(interval) * time.Second,
		}
		return daemon.Start(cfg)
	},
}

// _runCmd is the internal command that the forked daemon process runs
var _runCmd = &cobra.Command{
	Use:    "_run",
	Short:  "Internal: run daemon loop",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		interval, _ := cmd.Flags().GetInt("interval")
		port, _ := cmd.Flags().GetInt("port")

		cfg := &daemon.Config{
			Interval: time.Duration(interval) * time.Second,
			Port:     port,
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-sigCh
			cancel()
		}()

		return daemon.Run(ctx, cfg)
	},
}

func init() {
	_runCmd.Flags().Int("interval", 30, "Collection interval in seconds")
	_runCmd.Flags().Int("port", 9810, "HTTP server port")

	restartCmd.Flags().Int("port", 9810, "Web UI port")
	restartCmd.Flags().Int("interval", 30, "Collection interval in seconds")

	rootCmd.AddCommand(stopCmd, restartCmd, _runCmd)
}
