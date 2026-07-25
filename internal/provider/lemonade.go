package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

type Lemonade struct {
	name       string
	cfg        config.ProviderConfig
	httpClient *http.Client
}

var _ Lifecycle = (*Lemonade)(nil)

func NewLemonade(name string, cfg config.ProviderConfig) *Lemonade {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicitly configured
	}
	return &Lemonade{
		name:       name,
		cfg:        cfg,
		httpClient: &http.Client{Transport: transport},
	}
}

func (l *Lemonade) BaseURL() string          { return l.cfg.BaseURL }
func (l *Lemonade) InferenceAPIKey() string  { return l.cfg.ResolvedAPIKey }
func (l *Lemonade) HTTPClient() *http.Client { return l.httpClient }
func (l *Lemonade) Capabilities() Capabilities {
	return Capabilities{Discovery: true, Load: true, Unload: true, Pin: true, Inference: true}
}

func (l *Lemonade) Health(ctx context.Context) (Health, error) {
	var health Health
	if err := l.do(ctx, http.MethodGet, "/api/v1/health", nil, &health); err != nil {
		return Health{}, err
	}
	if health.Status != "ok" {
		return Health{}, Error{Provider: l.name, Code: "provider_unhealthy", Message: "health endpoint did not report ok"}
	}
	return health, nil
}

func (l *Lemonade) Discover(ctx context.Context) ([]DiscoveredModel, error) {
	var response struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := l.do(ctx, http.MethodGet, "/api/v1/models", nil, &response); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(response.Data))
	for _, item := range response.Data {
		if item.ID != "" {
			models = append(models, DiscoveredModel{ID: item.ID, OwnedBy: item.OwnedBy})
		}
	}
	return models, nil
}

func (l *Lemonade) Load(ctx context.Context, model string, pinned *bool) error {
	body := map[string]any{"model_name": model}
	if pinned != nil {
		body["pinned"] = *pinned
	}
	return l.doWithTimeout(ctx, http.MethodPost, "/api/v1/load", body, nil, l.cfg.ColdStartTimeout)
}

func (l *Lemonade) Unload(ctx context.Context, model string) error {
	return l.do(ctx, http.MethodPost, "/api/v1/unload", map[string]any{"model_name": model}, nil)
}

// Pin intentionally uses the public load endpoint. Current Lemonade releases
// treat an explicit pinned value on an already-loaded model as a dynamic pin
// update, avoiding the compatibility risk of /internal/pin.
func (l *Lemonade) Pin(ctx context.Context, model string, pinned bool) error {
	return l.do(ctx, http.MethodPost, "/api/v1/load", map[string]any{
		"model_name": model,
		"pinned":     pinned,
	}, nil)
}

func (l *Lemonade) do(ctx context.Context, method, path string, body any, out any) error {
	return l.doWithTimeout(ctx, method, path, body, out, l.cfg.ManagementTimeout)
}

func (l *Lemonade) doWithTimeout(ctx context.Context, method, path string, body any, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.cfg.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	key := l.cfg.ResolvedAdminAPIKey
	if key == "" {
		key = l.cfg.ResolvedAPIKey
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return Error{Provider: l.name, Code: "provider_unreachable", Message: "provider request failed", Status: http.StatusServiceUnavailable}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Error{Provider: l.name, Code: "invalid_provider_response", Message: "could not read provider response"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, _ := decodeLemonadeError(data)
		if code == "" {
			code = "provider_request_failed"
		}
		message := safeLemonadeMessage(code)
		status := http.StatusServiceUnavailable
		if resp.StatusCode == http.StatusConflict {
			status = http.StatusConflict
		}
		return Error{Provider: l.name, Code: code, Message: message, Status: status}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return Error{Provider: l.name, Code: "incompatible_provider_api", Message: "provider returned an unsupported response shape"}
		}
	}
	return nil
}

func safeLemonadeMessage(code string) string {
	switch code {
	case "slots_pinned_error":
		return "provider capacity is occupied by pinned models"
	case "model_not_found", "model_not_registered":
		return "provider model is not registered"
	default:
		return "provider lifecycle request failed"
	}
}

func decodeLemonadeError(data []byte) (string, string) {
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &response) != nil {
		return "", ""
	}
	message := response.Error.Message
	if message == "" {
		message = response.Message
	}
	// Do not relay arbitrary large backend details to an untrusted client.
	message = strings.TrimSpace(message)
	if len(message) > 300 {
		message = message[:300]
	}
	return response.Error.Code, message
}

func boolPtr(v bool) *bool { return &v }

func residentReady(model ResidentModel) bool {
	if model.Loaded != nil && !*model.Loaded {
		return false
	}
	switch model.Status {
	case "", "ready", "loaded":
		return true
	default:
		return false
	}
}

func validateLemonadeHealth(name string, health Health) error {
	if health.Status != "ok" {
		return fmt.Errorf("provider %s did not report healthy status", name)
	}
	if health.AllModelsLoaded == nil {
		health.AllModelsLoaded = []ResidentModel{}
	}
	return nil
}
