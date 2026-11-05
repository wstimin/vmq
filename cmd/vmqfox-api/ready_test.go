package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartupRequiresActualHTTPReadiness(t *testing.T) {
	for _, status := range []int{200, 503, 302} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ready" {
				t.Errorf("wrong probe path: %s", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		err := confirmHTTPReady(context.Background(), s.Listener.Addr().String())
		s.Close()
		if (err == nil) != (status == 200) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}
