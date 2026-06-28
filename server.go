package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type bridgeServer struct {
	cfg       config
	client    *http.Client
	startedAt time.Time
	semaphore chan struct{}
	active    atomic.Int64
	total     atomic.Int64
	failures  atomic.Int64
	metricsMu sync.RWMutex
	last      requestMetrics
	probeMu   sync.Mutex
	probe     probeResult
}

type requestMetrics struct {
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

type probeResult struct {
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Reachable bool      `json:"reachable"`
	Status    int       `json:"status,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func newBridgeServer(cfg config) *bridgeServer {
	return &bridgeServer{
		cfg:       cfg,
		client:    &http.Client{},
		startedAt: time.Now(),
		semaphore: make(chan struct{}, cfg.maxConcurrent),
	}
}

func (s *bridgeServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /health/details", s.requireAuth(s.handleHealthDetails))
	mux.HandleFunc("GET /v1/models", s.requireAuth(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.requireAuth(s.handleChat))
	return mux
}

func (s *bridgeServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.bridgeAPIKey == "" {
			next(w, r)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.bridgeAPIKey)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"message": "Unauthorized"}})
			return
		}
		next(w, r)
	}
}

func (s *bridgeServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"model":           modelAlias,
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
		"active_requests": s.active.Load(),
	})
}

func (s *bridgeServer) handleHealthDetails(w http.ResponseWriter, r *http.Request) {
	s.metricsMu.RLock()
	last := s.last
	s.metricsMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"model":           modelAlias,
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
		"active_requests": s.active.Load(),
		"requests_total":  s.total.Load(),
		"failures_total":  s.failures.Load(),
		"last_request":    last,
		"upstream":        s.probeUpstream(r.Context()),
		"limits": map[string]any{
			"max_concurrent":    s.cfg.maxConcurrent,
			"max_request_bytes": s.cfg.maxRequestBytes,
			"max_image_bytes":   s.cfg.maxImageBytes,
		},
		"capabilities": map[string]any{
			"chat":                 true,
			"stream":               true,
			"tools":                true,
			"images":               true,
			"reasonix_attachments": imageCapability(s.cfg),
		},
	})
}

func (s *bridgeServer) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []any{map[string]any{
			"id": modelAlias, "object": "model", "created": 0, "owned_by": "blackbox",
		}},
	})
}

func (s *bridgeServer) handleChat(w http.ResponseWriter, r *http.Request) {
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	default:
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{"message": "Bridge concurrency limit reached"}})
		return
	}

	s.active.Add(1)
	s.total.Add(1)
	defer s.active.Add(-1)
	started := time.Now()
	status, requestErr := s.proxyChat(w, r)
	s.recordRequest(status, time.Since(started), requestErr)
}

func (s *bridgeServer) proxyChat(w http.ResponseWriter, r *http.Request) (int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "request body too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"message": "Invalid request body"}})
		return status, err
	}

	var payload map[string]any
	decoderErr := json.Unmarshal(body, &payload)
	if decoderErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "Invalid JSON"}})
		return http.StatusBadRequest, decoderErr
	}
	payload["model"] = mapModel(payload["model"])
	if err := expandReasonixImages(payload, s.cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "Invalid image attachment"}})
		return http.StatusBadRequest, err
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "Invalid request"}})
		return http.StatusBadRequest, err
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.upstreamTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.upstreamBaseURL+"/v1/chat/completions", strings.NewReader(string(normalized)))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"message": "Upstream request failed"}})
		return http.StatusBadGateway, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.cfg.blackboxGatewayKey)
	request.Header.Set("userId", s.cfg.blackboxUserID)
	request.Header.Set("version", "1.1")

	response, err := s.client.Do(request)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"message": "Upstream request failed"}})
		return status, err
	}
	defer response.Body.Close()

	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	stream, _ := payload["stream"].(bool)
	if stream {
		err = copyStream(w, response.Body)
	} else {
		_, err = io.Copy(w, response.Body)
	}
	return response.StatusCode, err
}

func mapModel(value any) string {
	model, _ := value.(string)
	switch model {
	case "", modelAlias, "minimax-m2", upstreamModel:
		return upstreamModel
	default:
		return model
	}
}

func copyResponseHeaders(target, source http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control"} {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
}

func copyStream(w http.ResponseWriter, source io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		count, err := source.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *bridgeServer) recordRequest(status int, latency time.Duration, err error) {
	entry := requestMetrics{Status: status, LatencyMS: latency.Milliseconds(), At: time.Now().UTC()}
	if err != nil || status >= 400 {
		s.failures.Add(1)
		if err != nil {
			entry.Error = safeError(err)
		}
	}
	s.metricsMu.Lock()
	s.last = entry
	s.metricsMu.Unlock()
}

func (s *bridgeServer) probeUpstream(ctx context.Context) probeResult {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if time.Since(s.probe.CheckedAt) < 30*time.Second {
		return s.probe
	}
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(probeCtx, http.MethodHead, s.cfg.upstreamBaseURL+"/", nil)
	response, err := s.client.Do(request)
	result := probeResult{CheckedAt: time.Now().UTC(), LatencyMS: time.Since(started).Milliseconds()}
	if err != nil {
		result.Error = safeError(err)
	} else {
		result.Reachable = true
		result.Status = response.StatusCode
		response.Body.Close()
	}
	s.probe = result
	return result
}

func safeError(err error) string {
	message := err.Error()
	if len(message) > 300 {
		return message[:300]
	}
	return message
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "JSON encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func upstreamHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}
