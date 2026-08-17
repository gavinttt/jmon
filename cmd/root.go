package cmd

import (
	"os"
	"time"

	"jmon/internal/daemon"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "jmon",
	Short: "JVM process monitoring and diagnostics tool",
	Long: `jmon - JVM process monitoring and diagnostics tool.

  jmon                   Start daemon + web UI, monitor all Java processes
  jmon stop              Stop the running daemon`,
	RunE: func(cmd *cobra.Command, args []string) error {
		port, _ := cmd.Flags().GetInt("port")
		interval, _ := cmd.Flags().GetInt("interval")
		cfg := &daemon.Config{
			Port:     port,
			Interval: time.Duration(interval) * time.Second,
		}
		return daemon.Start(cfg)
	},
}

func init() {
	rootCmd.Flags().Int("port", 9810, "Web UI port")
	rootCmd.Flags().Int("interval", 30, "Collection interval in seconds")
}

// Execute runs the root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
