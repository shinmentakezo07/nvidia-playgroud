package main

// Inbound request-body size limit.
//
// The server deliberately does not authenticate inbound requests, and the chat
// handlers buffer JSON bodies in memory before parsing. Without a cap a single
// client can pin unbounded memory with one chunked upload, so the server-wide
// limit is enforced at the edge: oversized reads fail, the handler sees a
// *http.MaxBytesError, and the gateway answers 413 instead of buffering.

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	sdkginhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

// maxRequestBodyError renders the 413 body for an over-limit request body and
// aborts the chain. It reports whether it handled the error.
func maxRequestBodyError(c *gin.Context, err error) bool {
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		return false
	}
	// The unread remainder of the body is still in flight; the connection
	// cannot be reused, and the client may keep sending. Close it.
	c.Header("Connection", "close")
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, sdkginhandlers.ErrorResponse{
		Error: sdkginhandlers.ErrorDetail{
			Message: "request body too large",
			Type:    "invalid_request_error",
		},
	})
	return true
}

// limitRequestBody caps the inbound body at limit bytes; limit <= 0 disables
// the cap (the -max-body 0 case). A declared Content-Length above the limit is
// rejected before any read; chunked bodies fail on the read that crosses it.
func limitRequestBody(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		req := c.Request
		if limit <= 0 || req == nil || req.Body == nil || req.Body == http.NoBody {
			c.Next()
			return
		}
		if req.ContentLength > limit {
			c.Header("Connection", "close")
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, sdkginhandlers.ErrorResponse{
				Error: sdkginhandlers.ErrorDetail{
					Message: "request body too large",
					Type:    "invalid_request_error",
				},
			})
			return
		}
		req.Body = http.MaxBytesReader(c.Writer, req.Body, limit)
		c.Next()
	}
}
