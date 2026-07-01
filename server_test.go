package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMapModel(t *testing.T) {
	tests := map[string]string{
		"":                           minimaxUpstreamModel,
		minimaxModelAlias:            minimaxUpstreamModel,
		"minimax-m2":                 minimaxUpstreamModel,
		minimaxUpstreamModel:         minimaxUpstreamModel,
		legacyMinimaxModel:           minimaxUpstreamModel,
		kimiModelAlias:               kimiUpstreamModel,
		kimiUpstreamModel:            kimiUpstreamModel,
		"caller-provided-model-name": "caller-provided-model-name",
	}
	for input, want := range tests {
		if got := mapModel(input); got != want {
			t.Fatalf("mapModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestShouldFallbackKimi(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if !shouldFallbackKimi(status) {
			t.Fatalf("status %d should trigger Kimi fallback", status)
		}
	}
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnprocessableEntity} {
		if shouldFallbackKimi(status) {
			t.Fatalf("status %d should not trigger Kimi fallback", status)
		}
	}
}

func TestModelRoute(t *testing.T) {
	tests := map[string]string{
		"":                   minimaxModelAlias,
		minimaxModelAlias:    minimaxModelAlias,
		minimaxUpstreamModel: minimaxModelAlias,
		legacyMinimaxModel:   minimaxModelAlias,
		kimiModelAlias:       kimiModelAlias,
		kimiUpstreamModel:    kimiModelAlias,
		"other":              "",
	}
	for input, want := range tests {
		if got := modelRoute(input); got != want {
			t.Fatalf("modelRoute(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMinimaxUsesDedicatedUpstream(t *testing.T) {
	server := newBridgeServer(config{
		upstreamBaseURL:    "https://legacy.example",
		minimaxBaseURL:     "https://api.blackbox.ai",
		minimaxAPIKey:      "minimax-no-key-required",
		blackboxUserID:     "legacy-user",
		blackboxGatewayKey: "legacy-key",
		maxConcurrent:      1,
	})
	request, err := server.buildUpstreamRequest(t.Context(), minimaxModelAlias, []byte(`{"model":"blackboxai/minimax/minimax-m2.7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.String() != "https://api.blackbox.ai/v1/chat/completions" {
		t.Fatalf("Minimax URL = %q", request.URL.String())
	}
	if request.Header.Get("Authorization") != "Bearer minimax-no-key-required" {
		t.Fatalf("Minimax authorization was not set")
	}
	if request.Header.Get("userId") != "" || request.Header.Get("version") != "" {
		t.Fatalf("Minimax request must not include legacy identity headers")
	}
}

func TestKimiUsesLegacyUpstream(t *testing.T) {
	server := newBridgeServer(config{
		upstreamBaseURL:    "https://legacy.example",
		minimaxBaseURL:     "https://api.blackbox.ai",
		minimaxAPIKey:      "minimax-no-key-required",
		blackboxUserID:     "legacy-user",
		blackboxGatewayKey: "legacy-key",
		maxConcurrent:      1,
	})
	request, err := server.buildUpstreamRequest(t.Context(), kimiModelAlias, []byte(`{"model":"moonshotai/kimi-k2.6"}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.String() != "https://legacy.example/v1/chat/completions" {
		t.Fatalf("Kimi URL = %q", request.URL.String())
	}
	if request.Header.Get("Authorization") != "Bearer legacy-key" || request.Header.Get("userId") != "legacy-user" || request.Header.Get("version") != "1.1" {
		t.Fatalf("Kimi legacy headers were not preserved")
	}
}

func TestModelsExcludesDisabledKimi(t *testing.T) {
	server := newBridgeServer(config{maxConcurrent: 1})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d", recorder.Code)
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].ID != minimaxModelAlias {
		t.Fatalf("models response = %+v", response.Data)
	}
}

func TestDisabledKimiReturnsServiceUnavailable(t *testing.T) {
	server := newBridgeServer(config{maxConcurrent: 1, maxRequestBytes: 1024})
	body := []byte(`{"model":"blackbox/kimi-k2.6","messages":[{"role":"user","content":"hello"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("Kimi status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("model_route_disabled")) {
		t.Fatalf("Kimi response = %s", recorder.Body.String())
	}
}

func TestResponseModelExtraction(t *testing.T) {
	const model = "gpt-5.4-nano-2026-03-17"
	if got := responseModelFromJSON([]byte(`{"model":"` + model + `"}`)); got != model {
		t.Fatalf("responseModelFromJSON = %q, want %q", got, model)
	}
	if got := responseModelFromSSELine([]byte("data: {\"model\":\"" + model + "\"}\n")); got != model {
		t.Fatalf("responseModelFromSSELine = %q, want %q", got, model)
	}
	if got := responseModelFromSSELine([]byte("data: [DONE]\n")); got != "" {
		t.Fatalf("DONE model = %q, want empty", got)
	}
}
