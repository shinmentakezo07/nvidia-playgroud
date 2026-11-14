package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEffectivePoolSizeBumpsToMaxInflight(t *testing.T) {
	tests := []struct {
		name         string
		poolSize     int
		maxInflight  int
		auto         bool
		wantPoolSize int
		wantBumped   bool
	}{
		{"pool larger stays", 12, 9, true, 12, false},
		{"pool smaller bumps", 4, 9, true, 9, true},
		{"unlimited inflight no bump", 4, 0, true, 4, false},
		{"auto off no bump", 4, 9, false, 4, false},
		{"equal stays", 9, 9, true, 9, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, bumped := effectivePoolSize(tt.poolSize, tt.maxInflight, tt.auto)
			if got != tt.wantPoolSize || bumped != tt.wantBumped {
				t.Fatalf("effectivePoolSize(%d,%d,%v) = (%d,%v), want (%d,%v)",
					tt.poolSize, tt.maxInflight, tt.auto, got, bumped, tt.wantPoolSize, tt.wantBumped)
			}
		})
	}
}

func TestKeepAlivePingSendsRequest(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	stop := startKeepAlivePinger(&http.Client{Timeout: 5 * time.Second, Transport: tr}, srv.URL, 20*time.Millisecond)
	defer stop()

	select {
	case <-hits:
	case <-time.After(2 * time.Second):
		t.Fatal("pinger did not send a request")
	}
}
