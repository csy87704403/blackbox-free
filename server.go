package main

import (
	"bufio"
	"bytes"
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
	cfg        config
	client     *http.Client
	startedAt  time.Time
	semaphore  chan struct{}
	active     atomic.Int64
	total      atomic.Int64
	failures   atomic.Int64
	metricsMu  sync.RWMutex
	last       requestMetrics
	probeMu    sync.Mutex
	probe      probeResult
	routesMu   sync.RWMutex
	routeState map[string]routeResolution
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

type routeResolution struct {
	Route               string `json:"route"`
	LastResolvedModel   string `json:"last_resolved_model,omitempty"`
	LastResolvedAPIBase string `json:"last_resolved_api_base,omitempty"`
	ResolvedAt          string `json:"resolved_at,omitempty"`
}

func newBridgeServer(cfg config) *bridgeServer {
	return &bridgeServer{
		cfg:       cfg,
		client:    &http.Client{},
		startedAt: time.Now(),
		semaphore: make(chan struct{}, cfg.maxConcurrent),
		routeState: map[string]routeResolution{
			minimaxModelAlias: {Route: minimaxModelAlias},
			kimiModelAlias:    {Route: kimiModelAlias},
		},
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
		"model":           minimaxModelAlias,
		"models":          []string{minimaxModelAlias},
		"disabled_models": []string{kimiModelAlias},
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
		"model":           minimaxModelAlias,
		"models":          []string{minimaxModelAlias},
		"disabled_models": []string{kimiModelAlias},
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
		"active_requests": s.active.Load(),
		"requests_total":  s.total.Load(),
		"failures_total":  s.failures.Load(),
		"last_request":    last,
		"upstream":        s.probeUpstream(r.Context()),
		"routes":          s.routeStatuses(),
		"limits": map[string]any{
			"max_concurrent":    s.cfg.maxConcurrent,
			"max_request_bytes": s.cfg.maxRequestBytes,
			"max_image_bytes":   s.cfg.maxImageBytes,
		},
		"capabilities": map[string]any{
			"chat":                 true,
			"stream":               true,
			"tools":                true,
			"images":               false,
			"reasonix_attachments": imageCapability(s.cfg),
		},
		"route_capabilities": map[string]any{
			minimaxModelAlias: map[string]bool{"chat": true, "stream": true, "tools": true, "images": false},
		},
	})
}

func (s *bridgeServer) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []any{
			map[string]any{"id": minimaxModelAlias, "object": "model", "created": 0, "owned_by": "blackbox"},
		},
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
	requestedModel, _ := payload["model"].(string)
	if isKimiModel(requestedModel) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{
				"message": "The blackbox/kimi-k2.6 route is temporarily unavailable",
				"type":    "service_unavailable",
				"code":    "model_route_disabled",
			},
		})
		return http.StatusServiceUnavailable, errors.New("Kimi route is disabled")
	}
	route := modelRoute(requestedModel)
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
	response, err := s.sendUpstream(ctx, route, normalized)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, map[string]any{"error": map[string]string{"message": "Upstream request failed"}})
		return status, err
	}
	if isKimiModel(requestedModel) && shouldFallbackKimi(response.StatusCode) {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		payload["model"] = kimiFallbackModel
		normalized, err = json.Marshal(payload)
		if err == nil {
			response, err = s.sendUpstream(ctx, kimiModelAlias, normalized)
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{"message": "Upstream fallback request failed"}})
			return http.StatusBadGateway, err
		}
	}
	defer response.Body.Close()

	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	headerModel := strings.TrimSpace(response.Header.Get("x-litellm-model-id"))
	resolvedAPIBase := strings.TrimSpace(response.Header.Get("x-litellm-model-api-base"))
	if response.StatusCode >= 200 && response.StatusCode < 300 && headerModel != "" {
		s.updateRouteResolution(route, headerModel, resolvedAPIBase)
	}
	stream, _ := payload["stream"].(bool)
	if stream {
		err = copyStream(w, response.Body, func(model string) {
			if response.StatusCode >= 200 && response.StatusCode < 300 && headerModel == "" {
				s.updateRouteResolution(route, model, resolvedAPIBase)
			}
		})
	} else {
		var responseBody []byte
		responseBody, err = io.ReadAll(response.Body)
		if err == nil {
			if response.StatusCode >= 200 && response.StatusCode < 300 && headerModel == "" {
				s.updateRouteResolution(route, responseModelFromJSON(responseBody), resolvedAPIBase)
			}
			_, err = w.Write(responseBody)
		}
	}
	return response.StatusCode, err
}

func mapModel(value any) string {
	model, _ := value.(string)
	switch model {
	case "", minimaxModelAlias, "minimax-m2", minimaxUpstreamModel, legacyMinimaxModel:
		return minimaxUpstreamModel
	case kimiModelAlias, kimiUpstreamModel:
		return kimiUpstreamModel
	default:
		return model
	}
}

func isKimiModel(model string) bool {
	return model == kimiModelAlias || model == kimiUpstreamModel
}

func modelRoute(model string) string {
	switch model {
	case "", minimaxModelAlias, "minimax-m2", minimaxUpstreamModel, legacyMinimaxModel:
		return minimaxModelAlias
	case kimiModelAlias, kimiUpstreamModel:
		return kimiModelAlias
	default:
		return ""
	}
}

func shouldFallbackKimi(status int) bool {
	return status == http.StatusUnauthorized ||
		status == http.StatusForbidden ||
		status == http.StatusNotFound ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func (s *bridgeServer) sendUpstream(ctx context.Context, route string, body []byte) (*http.Response, error) {
	request, err := s.buildUpstreamRequest(ctx, route, body)
	if err != nil {
		return nil, err
	}
	return s.client.Do(request)
}

func (s *bridgeServer) buildUpstreamRequest(ctx context.Context, route string, body []byte) (*http.Request, error) {
	baseURL := s.cfg.upstreamBaseURL
	apiKey := s.cfg.blackboxGatewayKey
	if route == minimaxModelAlias {
		baseURL = s.cfg.minimaxBaseURL
		apiKey = s.cfg.minimaxAPIKey
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)
	if route != minimaxModelAlias {
		request.Header.Set("userId", s.cfg.blackboxUserID)
		request.Header.Set("version", "1.1")
	}
	return request, nil
}

func copyResponseHeaders(target, source http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control"} {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
}

func copyStream(w http.ResponseWriter, source io.Reader, onModel func(string)) error {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(source)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if model := responseModelFromSSELine(line); model != "" && onModel != nil {
				onModel(model)
			}
			if _, writeErr := w.Write(line); writeErr != nil {
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

func responseModelFromJSON(body []byte) string {
	var value struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value.Model)
}

func responseModelFromSSELine(line []byte) string {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return ""
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(data, []byte("[DONE]")) {
		return ""
	}
	return responseModelFromJSON(data)
}

func (s *bridgeServer) updateRouteResolution(route, model, apiBase string) {
	if route == "" || model == "" {
		return
	}
	s.routesMu.Lock()
	s.routeState[route] = routeResolution{
		Route:               route,
		LastResolvedModel:   model,
		LastResolvedAPIBase: apiBase,
		ResolvedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	s.routesMu.Unlock()
}

func (s *bridgeServer) routeStatuses() []routeResolution {
	s.routesMu.RLock()
	defer s.routesMu.RUnlock()
	return []routeResolution{s.routeState[minimaxModelAlias]}
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
	request, _ := http.NewRequestWithContext(probeCtx, http.MethodHead, s.cfg.minimaxBaseURL+"/", nil)
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
