package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithLoggingPreservesFlusher(t *testing.T) {
	rw := &flushingResponseWriter{ResponseWriter: httptest.NewRecorder()}
	handler := withLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("logging response writer does not expose http.Flusher")
		}
		flusher.Flush()
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/events", nil))

	if !rw.flushed {
		t.Fatal("logging response writer did not forward Flush")
	}
}

func TestWithLoggingDoesNotAddFlusher(t *testing.T) {
	handler := withLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Fatal("logging response writer exposes http.Flusher for a non-flushing writer")
		}
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(nonFlushingResponseWriter{ResponseWriter: httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/events", nil))
}

type flushingResponseWriter struct {
	http.ResponseWriter
	flushed bool
}

func (w *flushingResponseWriter) Flush() {
	w.flushed = true
}

type nonFlushingResponseWriter struct {
	http.ResponseWriter
}
