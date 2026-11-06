// Package providerhttp is the HTTP plumbing shared by provider adapters. Its
// job is to sort failures into transient (repeat with the same idempotency
// key) and definitive (do not repeat).
package providerhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tancon23/unified-payments-assessment/internal/gateway"
)

const maxResponseBytes = 1 << 20

// Client sends requests to one provider's API.
type Client struct {
	Provider string
	HTTP     *http.Client
	// ParseError extracts the provider's error code and message from the
	// body of a 4xx response.
	ParseError func(body []byte) (code, message string)
}

// Do sends req and decodes a 2xx JSON response into out, which may be nil.
func (c *Client) Do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		// Timeouts land here too: the provider may or may not have acted.
		return fmt.Errorf("%w: %s %s: %v", gateway.ErrTransient, req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: reading %s response: %v", gateway.ErrTransient, req.URL.Path, err)
	}

	switch {
	case transient(resp.StatusCode):
		return fmt.Errorf("%w: %s %s: HTTP %d", gateway.ErrTransient, req.Method, req.URL.Path, resp.StatusCode)
	case resp.StatusCode >= 400:
		code, msg := c.ParseError(body)
		return &gateway.RejectedError{Provider: c.Provider, StatusCode: resp.StatusCode, Code: code, Message: msg}
	case out == nil:
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// transient reports whether a status code leaves the outcome open. 409 is
// Stripe's answer while an earlier request with the same idempotency key is
// still being processed; repeating it later returns that request's result.
func transient(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}
