package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fastclaw-ai/weclaw-pusher/ilink"
	"github.com/spf13/cobra"
)

var (
	listenAddrFlag       string
	nonBlockingFlag      bool
	blockingTimeoutFlag  int    // seconds, 0 = infinite
	callbackURLsFlag      string // PUSH mode: comma-separated callback URLs
)

func init() {
	listenCmd.Flags().StringVar(&listenAddrFlag, "listen-addr", "127.0.0.1:18012", "Listen address for webhook API")
	listenCmd.Flags().BoolVarP(&nonBlockingFlag, "non-blocking", "n", false, "Non-blocking mode: return immediately with queued messages")
	listenCmd.Flags().IntVar(&blockingTimeoutFlag, "timeout", 60, "Blocking timeout in seconds (0 = infinite)")
	listenCmd.Flags().StringVar(&callbackURLsFlag, "callback-url", "", "Callback URL(s) to POST messages to (comma-separated for multiple, PUSH mode)")
	rootCmd.AddCommand(listenCmd)
}

var listenCmd = &cobra.Command{
	Use:   "listen",
	Short: "Listen for incoming WeChat messages and forward via webhook",
	Example: `  # Blocking mode (default): wait for messages
  weclaw-pusher listen

  # Non-blocking mode: return immediately with queued messages
  weclaw-pusher listen --non-blocking

  # PUSH mode: POST messages to single callback URL
  weclaw-pusher listen --callback-url http://example.com/webhook

  # PUSH mode: broadcast to multiple callback URLs (comma-separated)
  weclaw-pusher listen --callback-url "http://hook1.com,http://hook2.com"

  # Custom address
  weclaw-pusher listen --listen-addr 0.0.0.0:8080`,
	RunE: runListen,
}

type receivedMessage struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Type    int       `json:"type"`
	Text    string    `json:"text,omitempty"`
	Time    time.Time `json:"time"`
}

var (
	receivedMsgs []receivedMessage
	msgMutex     sync.RWMutex
	msgCond      *sync.Cond
	callbackURLs []string
)

// runListen starts the monitor and webhook server
func runListen(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Parse callback URLs
	if callbackURLsFlag != "" {
		callbackURLs = strings.Split(callbackURLsFlag, ",")
		for i := range callbackURLs {
			callbackURLs[i] = strings.TrimSpace(callbackURLs[i])
		}
		log.Printf("[listen] Callback URLs: %d configured", len(callbackURLs))
	}

	// Load all accounts
	accounts, err := ilink.LoadAllCredentials()
	if err != nil {
		return fmt.Errorf("failed to load credentials: %w", err)
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no accounts found, run 'weclaw-pusher login' first")
	}

	// Create client
	client := ilink.NewClient(accounts[0])

	// Initialize sync primitives
	msgCond = sync.NewCond(&sync.Mutex{})
	receivedMsgs = make([]receivedMessage, 0)

	// Create message handler that stores messages and optionally pushes to callbacks
	handler := func(ctx context.Context, c *ilink.Client, msg ilink.WeixinMessage) {
		rm := receivedMessage{
			From: msg.FromUserID,
			To:   msg.ToUserID,
			Type: msg.MessageType,
			Time: time.Now(),
		}
		// Extract text
		for _, item := range msg.ItemList {
			if item.Type == ilink.ItemTypeText && item.TextItem != nil {
				rm.Text = item.TextItem.Text
				break
			}
		}
		log.Printf("[listen] received: %s", ilink.FormatMessageSummary(msg))

		// PUSH mode: send to all callback URLs
		if len(callbackURLs) > 0 {
			for _, url := range callbackURLs {
				go pushToCallback(rm, url)
			}
		}

		msgCond.L.Lock()
		receivedMsgs = append(receivedMsgs, rm)
		msgCond.L.Unlock()
		msgCond.Signal()
	}

	// Create and start monitor
	monitor, err := ilink.NewMonitor(client, handler)
	if err != nil {
		return fmt.Errorf("create monitor: %w", err)
	}

	// Start monitor in background
	go func() {
		if err := monitor.Run(ctx); err != nil {
			log.Printf("[monitor] stopped: %v", err)
		}
	}()

	// Setup webhook server (only if no callback URLs)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/webhook", handleWebhook)
	mux.HandleFunc("/api/messages", handleMessages)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	addr := listenAddrFlag
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	mode := "blocking"
	if nonBlockingFlag {
		mode = "non-blocking"
	}
	if len(callbackURLs) > 0 {
		mode = fmt.Sprintf("PUSH to %d URL(s)", len(callbackURLs))
	}

	log.Printf("[listen] Starting (mode: %s)", mode)
	if len(callbackURLs) == 0 {
		log.Printf("[listen] Webhook: GET %s/api/webhook", addr)
	}
	log.Printf("[listen] Waiting for messages... (Ctrl+C to stop)")

	if len(callbackURLs) > 0 {
		// PUSH mode: don't start webhook server, just wait
		<-ctx.Done()
		return nil
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// pushToCallback POSTs a message to a single callback URL
func pushToCallback(msg receivedMessage, url string) {
	body, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[callback] failed to marshal message: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[callback] failed to create request for %s: %v", url, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[callback] POST to %s failed: %v", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[callback] POST to %s returned %d: %s", url, resp.StatusCode, string(body))
		return
	}

	log.Printf("[callback] message sent to %s", url)
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	msgCond.L.Lock()
	defer msgCond.L.Unlock()

	if nonBlockingFlag {
		// Non-blocking: return immediately
		msgs := make([]receivedMessage, len(receivedMsgs))
		copy(msgs, receivedMsgs)
		receivedMsgs = receivedMsgs[:0]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"count":   len(msgs),
			"messages": msgs,
		})
		return
	}

	// Blocking mode
	if blockingTimeoutFlag > 0 {
		// With timeout
		deadline := time.Now().Add(time.Duration(blockingTimeoutFlag) * time.Second)
		for len(receivedMsgs) == 0 && time.Now().Before(deadline) {
			msgCond.Wait()
		}
	} else {
		// Infinite wait
		for len(receivedMsgs) == 0 {
			msgCond.Wait()
		}
	}

	// Get all messages and clear
	msgs := make([]receivedMessage, len(receivedMsgs))
	copy(msgs, receivedMsgs)
	receivedMsgs = receivedMsgs[:0]

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"count":   len(msgs),
		"messages": msgs,
	})
}

// handleMessages is an alternative endpoint that always returns immediately
func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	msgCond.L.Lock()
	msgs := make([]receivedMessage, len(receivedMsgs))
	copy(msgs, receivedMsgs)
	receivedMsgs = receivedMsgs[:0]
	msgCond.L.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"count":   len(msgs),
		"messages": msgs,
	})
}