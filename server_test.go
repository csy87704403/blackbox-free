package main

import (
	"net/http"
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
