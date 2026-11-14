package main

import (
	"net/http"
	"time"
)

// effectivePoolSize returns the pool size to run with: under -auto, every
// concurrent in-flight stream can take one token at once, so a pool smaller
// than max-inflight makes callers wait for a mint (up to captcha-wait).
// max-inflight 0 means unlimited and cannot bound the pool; auto must be on.
func effectivePoolSize(poolSize, maxInflight int, auto bool) (int, bool) {
	if auto && maxInflight > 0 && poolSize < maxInflight {
		return maxInflight, true
	}
	return poolSize, false
}

// startKeepAlivePinger sends a cheap request over the shared transport every
// interval so the pooled predict connection never sits idle past
// IdleConnTimeout (the silent ~300-500ms reconnect on the first request after
// a quiet gap). Best-effort: failures are ignored; returns a stop func.
func startKeepAlivePinger(client *http.Client, url string, interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				req, err := http.NewRequest(http.MethodHead, url, nil)
				if err != nil {
					continue
				}
				resp, err := client.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}
		}
	}()
	return func() { close(done) }
}
