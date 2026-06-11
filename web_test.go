package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleAPISessionDetail_BadPath_Returns400(t *testing.T) {
	// /api/session/ with no agent/id should return 400, not panic
	req := httptest.NewRequest(http.MethodGet, "/api/session/", nil)
	w := httptest.NewRecorder()

	// Create a minimal mux to route the request
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session/", handleAPISessionDetail)
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected HTTP 400, got %d", w.Code)
	}
}
