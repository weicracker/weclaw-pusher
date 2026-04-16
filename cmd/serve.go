package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/fastclaw-ai/weclaw-pusher/api"
	"github.com/fastclaw-ai/weclaw-pusher/config"
	"github.com/fastclaw-ai/weclaw-pusher/ilink"
	"github.com/spf13/cobra"
)

var (
	apiAddrFlag    string
	daemonFlag     bool
)

func init() {
	serveCmd.Flags().StringVar(&apiAddrFlag, "api-addr", "", "API server listen address (default 127.0.0.1:18011)")
	serveCmd.Flags().BoolVarP(&daemonFlag, "daemon", "d", false, "Run in background")
	rootCmd.AddCommand(serveCmd)
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the HTTP API server for sending messages",
	RunE:  runServe,
}

func runServe(cmd *cobra.Command, args []string) error {
	if daemonFlag {
		return runDaemon()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load all accounts
	accounts, err := ilink.LoadAllCredentials()
	if err != nil {
		return fmt.Errorf("failed to load credentials: %w", err)
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no accounts found, run 'weclaw-pusher login' first")
	}

	// Create clients
	var clients []*ilink.Client
	for _, c := range accounts {
		clients = append(clients, ilink.NewClient(c))
	}

	// Resolve API addr: flag > env/config > default
	cfg, err := config.Load()
	if err != nil {
		log.Printf("Warning: failed to load config: %v", err)
	}

	apiAddr := cfg.APIAddr
	if apiAddrFlag != "" {
		apiAddr = apiAddrFlag
	}

	// Create and run server
	server := api.NewServer(clients, apiAddr)
	log.Printf("Starting API server on %s...", apiAddr)
	log.Printf("Accounts: %d", len(accounts))

	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

func runDaemon() error {
	// Ensure directory exists
	dir := weclawDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create weclaw dir: %w", err)
	}

	// Kill existing process
	if pid, err := readPid(); err == nil {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}

	// Re-exec
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	c := exec.Command(exe, "serve")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	if err := c.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	pid := c.Process.Pid
	os.WriteFile(pidFile(), []byte(strconv.Itoa(pid)), 0o644)

	fmt.Printf("weclaw-pusher started in background (pid=%d)\n", pid)
	fmt.Printf("Stop: weclaw-pusher stop\n")
	return nil
}

func weclawDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".weclaw")
}

func pidFile() string {
	return filepath.Join(weclawDir(), "weclaw-pusher.pid")
}

func readPid() (int, error) {
	data, err := os.ReadFile(pidFile())
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}