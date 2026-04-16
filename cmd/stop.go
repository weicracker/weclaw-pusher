package cmd

import (
	"fmt"
	"os"
	"syscall"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(stopCmd)
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the weclaw-pusher API server",
	RunE: func(cmd *cobra.Command, args []string) error {
		pidFilePath := pidFile()
		data, err := os.ReadFile(pidFilePath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("weclaw-pusher is not running")
				return nil
			}
			return fmt.Errorf("read pid file: %w", err)
		}

		var pid int
		if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
			return fmt.Errorf("parse pid: %w", err)
		}

		p, err := os.FindProcess(pid)
		if err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}

		os.Remove(pidFilePath)
		fmt.Println("weclaw-pusher stopped")
		return nil
	},
}