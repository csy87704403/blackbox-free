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
