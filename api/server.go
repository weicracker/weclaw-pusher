package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw-pusher/ilink"
	"github.com/fastclaw-ai/weclaw-pusher/messaging"
)

const (
	sessionExpiredBackoff = 5 * time.Second
	errCodeSessionExpired = -14
	keepAliveInterval     = 25 * time.Second
	keepAliveTimeout      = 20 * time.Second
	keepAliveBackoffBase  = 2 * time.Second
	keepAliveBackoffMax   = 30 * time.Second
)

// SendRequest is the JSON body for POST /api/send.
type SendRequest struct {
	To       string `json:"to"`
	Text     string `json:"text,omitempty"`
	MediaURL string `json:"media_url,omitempty"` // image/video/file URL
}

// Server provides an HTTP API for sending messages.
type Server struct {
	clients  []*ilink.Client
	addr     string
	syncBuf  string
	bufPath  string
	mu       sync.RWMutex
	stopChan chan struct{}
}

// NewServer creates an API server.
func NewServer(clients []*ilink.Client, addr string) *Server {
	if addr == "" {
		addr = "0.0.0.0:18011"
	}

	s := &Server{
		clients:  clients,
		addr:    addr,
		stopChan: make(chan struct{}),
	}

	// Initialize sync buf path and load existing buffer
	s.initSyncBuf()

	return s
}

// initSyncBuf initializes the sync buffer path and loads existing buffer.
func (s *Server) initSyncBuf() {
	if len(s.clients) == 0 {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	accountID := ilink.NormalizeAccountID(s.clients[0].BotID())
	s.bufPath = filepath.Join(home, ".weclaw", "accounts", accountID+".sync.json")
	s.loadSyncBuf()
}

// loadSyncBuf loads the sync buffer from disk.
func (s *Server) loadSyncBuf() {
	if s.bufPath == "" {
		return
	}
	data, err := os.ReadFile(s.bufPath)
	if err != nil {
		return
	}
	var bufData struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}
	if json.Unmarshal(data, &bufData) == nil && bufData.GetUpdatesBuf != "" {
		s.syncBuf = bufData.GetUpdatesBuf
		log.Printf("[api] loaded sync buf from %s", s.bufPath)
	}
}

// saveSyncBuf saves the sync buffer to disk.
func (s *Server) saveSyncBuf() {
	if s.bufPath == "" {
		return
	}
	dir := filepath.Dir(s.bufPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("[api] failed to create buf dir: %v", err)
		return
	}
	data, _ := json.Marshal(map[string]string{"get_updates_buf": s.syncBuf})
	if err := os.WriteFile(s.bufPath, data, 0o600); err != nil {
		log.Printf("[api] failed to save buf: %v", err)
	}
}

// Run starts the HTTP server and background keep-alive monitor. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{Addr: s.addr, Handler: mux}

	// Start background keep-alive monitor if we have clients
	if len(s.clients) > 0 {
		go s.keepAliveMonitor(ctx)
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	log.Printf("[api] listening on %s", s.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// keepAliveMonitor continuously polls GetUpdates to keep the session alive.
// This prevents the WeChat session from expiring when the server is idle.
// Uses exponential backoff on errors, similar to wcfLink's PollerManager.
func (s *Server) keepAliveMonitor(ctx context.Context) {
	log.Printf("[api] starting keep-alive monitor")

	backoff := keepAliveBackoffBase

	for {
		select {
		case <-ctx.Done():
			log.Printf("[api] keep-alive monitor stopped")
			return
		default:
		}

		// Perform keep-alive request
		shouldBackoff := s.doKeepAlive(ctx)

		if shouldBackoff {
			// Exponential backoff on errors
			log.Printf("[api] keep-alive backing off for %v", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > keepAliveBackoffMax {
				backoff = keepAliveBackoffMax
			}
		} else {
			// Reset backoff on successful keep-alive
			backoff = keepAliveBackoffBase
			// Wait before next keep-alive
			select {
			case <-ctx.Done():
				return
			case <-time.After(keepAliveInterval):
			}
		}
	}
}

// doKeepAlive performs a single keep-alive request.
// Returns true if the caller should back off (error occurred), false for success.
func (s *Server) doKeepAlive(ctx context.Context) bool {
	if len(s.clients) == 0 {
		return true
	}

	client := s.clients[0]

	// Use a shorter timeout for keep-alive requests
	keepAliveCtx, cancel := context.WithTimeout(ctx, keepAliveTimeout)
	defer cancel()

	resp, err := client.GetUpdates(keepAliveCtx, s.syncBuf)
	if err != nil {
		log.Printf("[api] keep-alive failed: %v", err)
		return true
	}

	// Handle session expired
	if resp.ErrCode == errCodeSessionExpired {
		if s.syncBuf != "" {
			log.Printf("[api] session expired during keep-alive, resetting sync buf")
			s.mu.Lock()
			s.syncBuf = ""
			s.saveSyncBuf()
			s.mu.Unlock()
		} else {
			log.Printf("[api] WARNING: WeChat session expired and cannot be auto-recovered. Run `weclaw-pusher login` to re-authenticate.")
		}
		// Wait before retrying
		select {
		case <-time.After(sessionExpiredBackoff):
		case <-ctx.Done():
		}
		return true
	}

	// Update sync buf if changed
	if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != s.syncBuf {
		s.mu.Lock()
		s.syncBuf = resp.GetUpdatesBuf
		s.saveSyncBuf()
		s.mu.Unlock()
	}

	log.Printf("[api] keep-alive successful")
	return false
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.To == "" {
		http.Error(w, `"to" is required`, http.StatusBadRequest)
		return
	}
	if req.Text == "" && req.MediaURL == "" {
		http.Error(w, `"text" or "media_url" is required`, http.StatusBadRequest)
		return
	}

	if len(s.clients) == 0 {
		http.Error(w, "no accounts configured", http.StatusServiceUnavailable)
		return
	}

	// Use the first client
	client := s.clients[0]
	ctx := r.Context()

	// Send text if provided
	if req.Text != "" {
		if err := messaging.SendTextReply(ctx, client, req.To, req.Text, "", ""); err != nil {
			log.Printf("[api] send text failed: %v", err)
			http.Error(w, "send text failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[api] sent text to %s: %q", req.To, req.Text)

		// Extract and send any markdown images embedded in text
		for _, imgURL := range messaging.ExtractImageURLs(req.Text) {
			if err := messaging.SendMediaFromURL(ctx, client, req.To, imgURL, ""); err != nil {
				log.Printf("[api] send extracted image failed: %v", err)
			} else {
				log.Printf("[api] sent extracted image to %s: %s", req.To, imgURL)
			}
		}
	}

	// Send media if provided
	if req.MediaURL != "" {
		if err := messaging.SendMediaFromURL(ctx, client, req.To, req.MediaURL, ""); err != nil {
			log.Printf("[api] send media failed: %v", err)
			http.Error(w, "send media failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[api] sent media to %s: %s", req.To, req.MediaURL)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}