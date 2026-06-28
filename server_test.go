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
