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
	MediaURL string `json:"media_url,omitempty"`
}

// Server provides an HTTP API for sending messages.
type Server struct {
	clients        []*ilink.Client
	addr           string
	syncBufs       map[string]string
	bufPaths       map[string]string
	mu             sync.RWMutex
	stopChan       chan struct{}
	contextTokenMap sync.Map
	userToBot      sync.Map // key=userID, value=botID
	clientDead     sync.Map // key=botID, value=bool
}

// NewServer creates an API server.
func NewServer(clients []*ilink.Client, addr string) *Server {
	if addr == "" {
		addr = "0.0.0.0:18011"
	}
	s := &Server{
		clients:     clients,
		syncBufs:    make(map[string]string),
		bufPaths:    make(map[string]string),
		addr:        addr,
		stopChan:    make(chan struct{}),
	}
	for _, c := range clients {
		log.Printf("[api] registered bot_id=%s user_id=%s", c.BotID(), c.UserID())
	}
	s.initSyncBufs()
	return s
}

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
	os.WriteFile(bufPath, data, 0o600)
}

func (s *Server) selectClient(toUserID string) (*ilink.Client, error) {
	if botID, ok := s.userToBot.Load(toUserID); ok {
		botIDStr := botID.(string)
		for _, c := range s.clients {
			if c.BotID() == botIDStr {
				return c, nil
			}
		}
	}
	if len(s.clients) == 1 {
		return s.clients[0], nil
	}
	if len(s.clients) > 1 {
		return nil, fmt.Errorf("user %s not yet routed: they must send a message to the bot first", toUserID)
	}
	return nil, fmt.Errorf("no accounts configured")
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: s.addr, Handler: mux}
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
}

func (s *Server) keepAliveForClient(ctx context.Context, client *ilink.Client) {
	botID := client.BotID()
	log.Printf("[api] keep-alive started for bot %s", botID)
	backoff := keepAliveBackoffBase
	for {
		select {
		case <-ctx.Done():
			log.Printf("[api] keep-alive stopped for bot %s", botID)
			return
		default:
		}
		if dead, ok := s.clientDead.Load(botID); ok && dead.(bool) {
			log.Printf("[api] keep-alive stopped for bot %s: session dead", botID)
			return
		}
		shouldBackoff := s.doKeepAliveForClient(ctx, client)
		if !shouldBackoff {
			if dead, ok := s.clientDead.Load(botID); ok && dead.(bool) {
				return
			}
			backoff = keepAliveBackoffBase
			select {
			case <-ctx.Done():
				return
			case <-time.After(keepAliveInterval):
			}
		} else {
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
	if resp.ErrCode == errCodeSessionExpired {
		if syncBuf != "" {
			s.mu.Lock()
			s.syncBufs[botID] = ""
			s.saveSyncBuf(botID)
			s.mu.Unlock()
		} else {
			log.Printf("[api] WARNING: bot %s session expired", botID)
			s.clientDead.Store(botID, true)
			return false
		}
		s.clearAllContextTokens()
		select {
		case <-time.After(sessionExpiredBackoff):
		case <-ctx.Done():
		}
		return true
	}
	if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != syncBuf {
		s.mu.Lock()
		s.syncBufs[botID] = resp.GetUpdatesBuf
		s.saveSyncBuf(botID)
		s.mu.Unlock()
	}
	for _, msg := range resp.Msgs {
		s.userToBot.Store(msg.FromUserID, botID)
		if msg.ContextToken != "" {
			s.contextTokenMap.Store(msg.FromUserID, msg.ContextToken)
		}
	}
	return false
}

func (s *Server) clearAllContextTokens() {
	s.contextTokenMap.Range(func(key, value interface{}) bool {
		s.contextTokenMap.Delete(key)
		return true
	})
}

func (s *Server) getContextToken(userID string) string {
	if val, ok := s.contextTokenMap.Load(userID); ok {
		return val.(string)
	}
	return ""
}

func (s *Server) clearContextToken(userID string) {
	s.contextTokenMap.Delete(userID)
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
	client, err := s.selectClient(req.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if dead, ok := s.clientDead.Load(client.BotID()); ok && dead.(bool) {
		http.Error(w, "session expired, please re-login", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	contextToken := s.getContextToken(req.To)
	if req.Text != "" {
		if err := messaging.SendTextReply(ctx, client, req.To, req.Text, contextToken, ""); err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "ret=-2") {
				s.clearContextToken(req.To)
				http.Error(w, "send failed: context_token invalid or expired", http.StatusInternalServerError)
				return
			}
			http.Error(w, "send failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, imgURL := range messaging.ExtractImageURLs(req.Text) {
			messaging.SendMediaFromURL(ctx, client, req.To, imgURL, contextToken)
		}
	}
	if req.MediaURL != "" {
		if err := messaging.SendMediaFromURL(ctx, client, req.To, req.MediaURL, contextToken); err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "ret=-2") {
				s.clearContextToken(req.To)
				http.Error(w, "send failed: context_token invalid or expired", http.StatusInternalServerError)
				return
			}
			http.Error(w, "send failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}