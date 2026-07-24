package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Error is safe to return to clients: it identifies the provider and a stable
// code without exposing its URL, credentials, or raw response body.
type Error struct {
	Provider string
	Code     string
	Message  string
	Status   int
}

func (e Error) Error() string {
	return fmt.Sprintf("provider %s: %s", e.Provider, e.Message)
}

func (e Error) StatusCode() int {
	if e.Status != 0 {
		return e.Status
	}
	return http.StatusServiceUnavailable
}

func (e Error) Header() http.Header {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return h
}

func (e Error) Body() []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"type":    "llemon_provider_error",
			"code":    e.Code,
			"message": e.Message,
		},
	})
	return body
}
