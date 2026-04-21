package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	clients          []*ilink.Client
	userClients      map[string]*ilink.Client // key=ilinkUserID, value=client
	addr             string
	syncBufs         map[string]string        // key=botID, value=syncBuf per client
	bufPaths         map[string]string        // key=botID, value=bufPath per client
	mu               sync.RWMutex
	stopChan         chan struct{}
	contextTokenMap  sync.Map     // key=userID, value=contextToken
	sessionDead      atomic.Bool  // marks if session is dead (bot_token expired)
	clientDead       sync.Map     // key=botID, value=bool - per-client session dead state
}

// NewServer creates an API server.
func NewServer(clients []*ilink.Client, addr string) *Server {
	if addr == "" {
		addr = "0.0.0.0:18011"
	}

	s := &Server{
		clients:     clients,
		userClients: make(map[string]*ilink.Client),
		syncBufs:    make(map[string]string),
		bufPaths:    make(map[string]string),
		addr:        addr,
		stopChan:    make(chan struct{}),
	}

	// Build user ID -> client mapping
	for _, c := range clients {
		if c.UserID() != "" {
			s.userClients[c.UserID()] = c
			log.Printf("[api] mapped ilink_user_id=%s to bot_id=%s", c.UserID(), c.BotID())
		}
	}

	// Initialize sync buf paths and load existing buffers for all clients
	s.initSyncBufs()

	return s
}

// initSyncBufs initializes sync buffer paths and loads existing buffers for all clients.
func (s *Server) initSyncBufs() {
	if len(s.clients) == 0 {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, c := range s.clients {
		accountID := ilink.NormalizeAccountID(c.BotID())
		bufPath := filepath.Join(home, ".weclaw", "accounts", accountID+".sync.json")
		s.bufPaths[c.BotID()] = bufPath

		data, err := os.ReadFile(bufPath)
		if err != nil {
			continue
		}
		var bufData struct {
			GetUpdatesBuf string `json:"get_updates_buf"`
		}
		if json.Unmarshal(data, &bufData) == nil && bufData.GetUpdatesBuf != "" {
			s.syncBufs[c.BotID()] = bufData.GetUpdatesBuf
			log.Printf("[api] loaded sync buf for bot %s from %s", c.BotID(), bufPath)
		}
	}
}

// saveSyncBuf saves the sync buffer to disk for a specific bot.
func (s *Server) saveSyncBuf(botID string) {
	bufPath, ok := s.bufPaths[botID]
	if !ok || bufPath == "" {
		return
	}
	syncBuf := s.syncBufs[botID]
	dir := filepath.Dir(bufPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("[api] failed to create buf dir: %v", err)
		return
	}
	data, _ := json.Marshal(map[string]string{"get_updates_buf": syncBuf})
	if err := os.WriteFile(bufPath, data, 0o600); err != nil {
		log.Printf("[api] failed to save buf: %v", err)
	}
}

// selectClient selects the correct client for sending a message to the given user ID.
// It first tries to match by ilink_user_id (the logged-in WeChat user),
// then falls back to the first client if only one account exists.
func (s *Server) selectClient(toUserID string) (*ilink.Client, error) {
	// Try to find client by ilink_user_id mapping
	if client, ok := s.userClients[toUserID]; ok {
		return client, nil
	}

	// If only one client, use it (backward compatible with single-account setup)
	if len(s.clients) == 1 {
		return s.clients[0], nil
	}

	// Multiple clients but no match found
	return nil, fmt.Errorf("no account found for user %s (logged-in accounts: %d)", toUserID, len(s.clients))
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

// keepAliveMonitor continuously polls GetUpdates to keep all sessions alive.
// This prevents WeChat sessions from expiring when the server is idle.
// Each client gets its own keep-alive goroutine.
func (s *Server) keepAliveMonitor(ctx context.Context) {
	log.Printf("[api] starting keep-alive monitor for %d client(s)", len(s.clients))

	var wg sync.WaitGroup
	for _, client := range s.clients {
		wg.Add(1)
		go func(c *ilink.Client) {
			defer wg.Done()
			s.keepAliveForClient(ctx, c)
		}(client)
	}
	wg.Wait()
	log.Printf("[api] all keep-alive monitors stopped")
}

// keepAliveForClient runs keep-alive loop for a single client.
func (s *Server) keepAliveForClient(ctx context.Context, client *ilink.Client) {
	botID := client.BotID()
	log.Printf("[api] keep-alive started for bot %s (user %s)", botID, client.UserID())

	backoff := keepAliveBackoffBase

	for {
		select {
		case <-ctx.Done():
			log.Printf("[api] keep-alive stopped for bot %s", botID)
			return
		default:
		}

		// Check if this client's session is dead
		if dead, ok := s.clientDead.Load(botID); ok && dead.(bool) {
			log.Printf("[api] keep-alive stopped for bot %s: session dead", botID)
			return
		}

		// Perform keep-alive request
		shouldBackoff := s.doKeepAliveForClient(ctx, client)

		if !shouldBackoff {
			if dead, ok := s.clientDead.Load(botID); ok && dead.(bool) {
				log.Printf("[api] keep-alive stopped for bot %s: session dead, please re-login", botID)
				return
			}
			backoff = keepAliveBackoffBase
			select {
			case <-ctx.Done():
				return
			case <-time.After(keepAliveInterval):
			}
		} else {
			log.Printf("[api] keep-alive backing off for bot %s: %v", botID, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > keepAliveBackoffMax {
				backoff = keepAliveBackoffMax
			}
		}
	}
}

// doKeepAliveForClient performs a single keep-alive request for a specific client.
// Returns true if the caller should back off (error occurred), false for success.
func (s *Server) doKeepAliveForClient(ctx context.Context, client *ilink.Client) bool {
	botID := client.BotID()

	keepAliveCtx, cancel := context.WithTimeout(ctx, keepAliveTimeout)
	defer cancel()

	syncBuf := s.syncBufs[botID]
	resp, err := client.GetUpdates(keepAliveCtx, syncBuf)
	if err != nil {
		log.Printf("[api] keep-alive failed for bot %s: %v", botID, err)
		return true
	}

	// Handle session expired
	if resp.ErrCode == errCodeSessionExpired {
		if syncBuf != "" {
			log.Printf("[api] session expired for bot %s, resetting sync buf", botID)
			s.mu.Lock()
			s.syncBufs[botID] = ""
			s.saveSyncBuf(botID)
			s.mu.Unlock()
		} else {
			log.Printf("[api] WARNING: bot %s session expired and cannot be auto-recovered. Run `weclaw-pusher login` to re-authenticate.", botID)
			s.clientDead.Store(botID, true)
			return false
		}
		// Clear all context_token cache
		s.clearAllContextTokens()
		select {
		case <-time.After(sessionExpiredBackoff):
		case <-ctx.Done():
		}
		return true
	}

	// Update sync buf if changed
	if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != syncBuf {
		s.mu.Lock()
		s.syncBufs[botID] = resp.GetUpdatesBuf
		s.saveSyncBuf(botID)
		s.mu.Unlock()
	}

	// Extract and cache context_token from received messages
	for _, msg := range resp.Msgs {
		if msg.ContextToken != "" {
			s.contextTokenMap.Store(msg.FromUserID, msg.ContextToken)
			log.Printf("[api] cached context_token for user %s (from bot %s)", msg.FromUserID, botID)
		}
	}

	log.Printf("[api] keep-alive successful for bot %s", botID)
	return false
}

// clearAllContextTokens clears all cached context_tokens.
func (s *Server) clearAllContextTokens() {
	s.contextTokenMap.Range(func(key, value interface{}) bool {
		s.contextTokenMap.Delete(key)
		return true
	})
	log.Printf("[api] cleared all context_token cache")
}

// getContextToken retrieves the cached context_token for a user.
// Returns empty string if not found.
func (s *Server) getContextToken(userID string) string {
	if val, ok := s.contextTokenMap.Load(userID); ok {
		return val.(string)
	}
	return ""
}

// clearContextToken removes the cached context_token for a user.
func (s *Server) clearContextToken(userID string) {
	s.contextTokenMap.Delete(userID)
	log.Printf("[api] cleared context_token for user %s", userID)
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

	// Check if all sessions are dead
	allDead := true
	for _, c := range s.clients {
		if dead, ok := s.clientDead.Load(c.BotID()); !ok || !dead.(bool) {
			allDead = false
			break
		}
	}
	if allDead {
		http.Error(w, "all sessions expired, please re-login", http.StatusServiceUnavailable)
		return
	}

	// Select the correct client based on the recipient's user ID
	client, err := s.selectClient(req.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Check if this specific client's session is dead
	if dead, ok := s.clientDead.Load(client.BotID()); ok && dead.(bool) {
		http.Error(w, fmt.Sprintf("session expired for bot %s, please re-login", client.BotID()), http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Get cached context_token for the recipient
	contextToken := s.getContextToken(req.To)
	if contextToken != "" {
		log.Printf("[api] using cached context_token for %s (via bot %s)", req.To, client.BotID())
	}

	// Send text if provided
	if req.Text != "" {
		if err := messaging.SendTextReply(ctx, client, req.To, req.Text, contextToken, ""); err != nil {
			// Check for ret=-2 error (invalid context_token)
			errMsg := err.Error()
			if strings.Contains(errMsg, "ret=-2") {
				log.Printf("[api] send failed with ret=-2, clearing context_token for %s", req.To)
				s.clearContextToken(req.To)
				http.Error(w, "send failed: context_token invalid or expired, please try again or re-login", http.StatusInternalServerError)
				return
			}
			log.Printf("[api] send text failed: %v", err)
			http.Error(w, "send text failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[api] sent text to %s via bot %s: %q", req.To, client.BotID(), req.Text)

		// Extract and send any markdown images embedded in text
		for _, imgURL := range messaging.ExtractImageURLs(req.Text) {
			if err := messaging.SendMediaFromURL(ctx, client, req.To, imgURL, contextToken); err != nil {
				log.Printf("[api] send extracted image failed: %v", err)
			} else {
				log.Printf("[api] sent extracted image to %s: %s", req.To, imgURL)
			}
		}
	}

	// Send media if provided
	if req.MediaURL != "" {
		if err := messaging.SendMediaFromURL(ctx, client, req.To, req.MediaURL, contextToken); err != nil {
			// Check for ret=-2 error (invalid context_token)
			errMsg := err.Error()
			if strings.Contains(errMsg, "ret=-2") {
				log.Printf("[api] send media failed with ret=-2, clearing context_token for %s", req.To)
				s.clearContextToken(req.To)
				http.Error(w, "send failed: context_token invalid or expired, please try again or re-login", http.StatusInternalServerError)
				return
			}
			log.Printf("[api] send media failed: %v", err)
			http.Error(w, "send media failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[api] sent media to %s via bot %s: %s", req.To, client.BotID(), req.MediaURL)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}