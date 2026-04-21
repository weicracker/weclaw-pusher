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
	blockingTimeoutFlag  int
	callbackURLsFlag     string
	accountCallbackFlag  string
)

func init() {
	listenCmd.Flags().StringVar(&listenAddrFlag, "listen-addr", "127.0.0.1:18012", "Listen address for webhook API")
	listenCmd.Flags().BoolVarP(&nonBlockingFlag, "non-blocking", "n", false, "Non-blocking mode: return immediately with queued messages")
	listenCmd.Flags().IntVar(&blockingTimeoutFlag, "timeout", 60, "Blocking timeout in seconds (0 = infinite)")
	listenCmd.Flags().StringVar(&callbackURLsFlag, "callback-url", "", "Callback URL(s) to POST messages to (comma-separated for multiple, PUSH mode)")
	listenCmd.Flags().StringVar(&accountCallbackFlag, "account-callback", "", "Per-account callback URLs (format: 0=http://url1,1=http://url2)")
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

  # PUSH mode: per-account callbacks
  weclaw-pusher listen --account-callback "0=http://hook1.com,1=http://hook2.com"

  # Custom address
  weclaw-pusher listen --listen-addr 0.0.0.0:8080`,
	RunE: runListen,
}

type receivedMessage struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	Type  int       `json:"type"`
	Text  string    `json:"text,omitempty"`
	Time  time.Time `json:"time"`
	BotID string    `json:"bot_id,omitempty"`
}

var (
	receivedMsgs []receivedMessage
	msgMutex     sync.RWMutex
	msgCond      *sync.Cond
	callbackURLs []string
)

func runListen(cmd *cobra.Command, args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if callbackURLsFlag != "" {
		callbackURLs = strings.Split(callbackURLsFlag, ",")
		for i := range callbackURLs {
			callbackURLs[i] = strings.TrimSpace(callbackURLs[i])
		}
		log.Printf("[listen] Global callback URLs: %d configured", len(callbackURLs))
	}

	accountCallbacks := make(map[int]string)
	if accountCallbackFlag != "" {
		pairs := strings.Split(accountCallbackFlag, ",")
		for _, pair := range pairs {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) == 2 {
				var idx int
				if _, err := fmt.Sscanf(kv[0], "%d", &idx); err == nil {
					accountCallbacks[idx] = strings.TrimSpace(kv[1])
					log.Printf("[listen] Account %d callback: %s", idx, accountCallbacks[idx])
				}
			}
		}
	}

	accounts, err := ilink.LoadAllCredentials()
	if err != nil {
		return fmt.Errorf("failed to load credentials: %w", err)
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no accounts found, run 'weclaw-pusher login' first")
	}
	log.Printf("[listen] Found %d account(s)", len(accounts))

	msgCond = sync.NewCond(&sync.Mutex{})
	receivedMsgs = make([]receivedMessage, 0)

	makeHandler := func(accountIndex int, callbackURL string) func(ctx context.Context, c *ilink.Client, msg ilink.WeixinMessage) {
		return func(ctx context.Context, c *ilink.Client, msg ilink.WeixinMessage) {
			rm := receivedMessage{
				From:  msg.FromUserID,
				To:    msg.ToUserID,
				Type:  msg.MessageType,
				Time:  time.Now(),
				BotID: c.BotID(),
			}
			for _, item := range msg.ItemList {
				if item.Type == ilink.ItemTypeText && item.TextItem != nil {
					rm.Text = item.TextItem.Text
					break
				}
			}
			log.Printf("[listen] received from account %d: %s", accountIndex, ilink.FormatMessageSummary(msg))

			if callbackURL != "" {
				go pushToCallback(rm, callbackURL)
			}

			msgCond.L.Lock()
			receivedMsgs = append(receivedMsgs, rm)
			msgCond.L.Unlock()
			msgCond.Signal()
		}
	}

	var wg sync.WaitGroup
	for i, cred := range accounts {
		client := ilink.NewClient(cred)

		callbackURL := ""
		if cb, ok := accountCallbacks[i]; ok {
			callbackURL = cb
		} else if len(callbackURLs) > 0 {
			callbackURL = callbackURLs[i%len(callbackURLs)]
		}
		if callbackURL != "" {
			log.Printf("[listen] Account %d (%s) will push to: %s", i, client.BotID(), callbackURL)
		}

		handler := makeHandler(i, callbackURL)
		monitor, err := ilink.NewMonitor(client, handler)
		if err != nil {
			log.Printf("[listen] failed to create monitor for account %d: %v", i, err)
			continue
		}

		wg.Add(1)
		go func(idx int, m *ilink.Monitor) {
			defer wg.Done()
			if err := m.Run(ctx); err != nil {
				log.Printf("[listen] monitor stopped for account %d: %v", idx, err)
			}
		}(i, monitor)
	}

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
	if len(callbackURLs) > 0 || len(accountCallbacks) > 0 {
		mode = "PUSH"
	}

	log.Printf("[listen] Starting %s mode", mode)
	if len(callbackURLs) == 0 && len(accountCallbacks) == 0 {
		log.Printf("[listen] Webhook: GET %s/api/webhook", addr)
	}
	log.Printf("[listen] Waiting for messages... (Ctrl+C to stop)")

	if len(callbackURLs) > 0 || len(accountCallbacks) > 0 {
		<-ctx.Done()
		wg.Wait()
		return nil
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	wg.Wait()
	return nil
}

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
		msgs := make([]receivedMessage, len(receivedMsgs))
		copy(msgs, receivedMsgs)
		receivedMsgs = receivedMsgs[:0]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"count":    len(msgs),
			"messages": msgs,
		})
		return
	}

	if blockingTimeoutFlag > 0 {
		deadline := time.Now().Add(time.Duration(blockingTimeoutFlag) * time.Second)
		for len(receivedMsgs) == 0 && time.Now().Before(deadline) {
			msgCond.Wait()
		}
	} else {
		for len(receivedMsgs) == 0 {
			msgCond.Wait()
		}
	}

	msgs := make([]receivedMessage, len(receivedMsgs))
	copy(msgs, receivedMsgs)
	receivedMsgs = receivedMsgs[:0]

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"count":    len(msgs),
		"messages": msgs,
	})
}

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
		"status":   "ok",
		"count":    len(msgs),
		"messages": msgs,
	})
}