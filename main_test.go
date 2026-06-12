package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithLoggingPreservesFlusher(t *testing.T) {
	handler := withLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("logging response writer does not expose http.Flusher")
		}
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))
}
