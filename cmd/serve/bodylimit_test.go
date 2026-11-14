package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestLimitRequestBody checks the two oversized-body shapes: a declared
// Content-Length over the limit is refused before any read, and a chunked body
// that crosses the limit mid-read is refused as it does, while a body exactly
// at the limit reaches the handler intact.
func TestLimitRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = 1024

	// handler mimics cliproxy: it drains the body, stalling like the real parser
	// would have before the limit was in place.
	handler := func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			if !maxRequestBodyError(c, err) {
				c.AbortWithStatus(http.StatusInternalServerError)
			}
			return
		}
		c.String(http.StatusOK, "%d", len(raw))
	}

	newServer := func() *httptest.Server {
		engine := gin.New()
		engine.Use(limitRequestBody(limit))
		engine.POST("/v1/chat/completions", handler)
		return httptest.NewServer(engine)
	}

	t.Run("declared oversized", func(t *testing.T) {
		srv := newServer()
		defer srv.Close()
		body := strings.Repeat("a", limit+1)
		resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("chunked oversized", func(t *testing.T) {
		srv := newServer()
		defer srv.Close()
		// io.Pipe with no Content-Length: the request is sent chunked.
		pr, pw := io.Pipe()
		go func() {
			// Keep writing until the server closes the connection.
			chunk := []byte(strings.Repeat("b", 256))
			for range 64 {
				if _, err := pw.Write(chunk); err != nil {
					pw.CloseWithError(err)
					return
				}
			}
			pw.Close()
		}()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", pr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if req.ContentLength != 0 {
			t.Fatalf("ContentLength = %d, want unknown for chunked", req.ContentLength)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			// A write racing the server's connection close is acceptable; the
			// server-side assertion is that it answered 413.
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("at limit passes", func(t *testing.T) {
		srv := newServer()
		defer srv.Close()
		body := strings.Repeat("c", limit)
		resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || string(raw) != "1024" {
			t.Fatalf("status = %d body = %q", resp.StatusCode, raw)
		}
	})
}

// TestFunctionPinOversizedBody checks that the pin middleware turns an
// over-limit body into 413 instead of falling through as a partial read.
func TestFunctionPinOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = 512
	engine := gin.New()
	engine.Use(limitRequestBody(limit))
	engine.Use(functionPinMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		t.Error("handler reached with an oversized body")
	})
	srv := httptest.NewServer(engine)
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		chunk := []byte(strings.Repeat("d", 256))
		for range 16 {
			if _, err := pw.Write(chunk); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}
