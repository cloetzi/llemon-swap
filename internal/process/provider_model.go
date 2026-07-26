package process

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/provider"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// ProviderModel adapts a model on a long-lived provider to the existing
// Process contract, allowing the router/scheduler to stay provider-agnostic.
type ProviderModel struct {
	id      string
	manager *provider.Manager
	logger  *logmon.Monitor
	proxy   *httputil.ReverseProxy
}

var _ Process = (*ProviderModel)(nil)
var _ SwapAware = (*ProviderModel)(nil)
var _ LeaseAware = (*ProviderModel)(nil)
var _ QueueAware = (*ProviderModel)(nil)

func NewProviderModel(id string, manager *provider.Manager, logger *logmon.Monitor) (*ProviderModel, error) {
	target, err := url.Parse(manager.Client().BaseURL())
	if err != nil {
		return nil, err
	}
	reverseProxy := &httputil.ReverseProxy{
		Transport: manager.Client().HTTPClient().Transport,
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			proxyReq.SetURL(target)
			proxyReq.SetXForwarded()
			// The browser origin terminates at llemon-swap. Forwarding it would
			// make Lemonade apply its browser-origin policy to this trusted
			// server-to-server hop.
			proxyReq.Out.Header.Del("Origin")
			// Client credentials authorize llemon-swap, not the provider.
			proxyReq.Out.Header.Del("Authorization")
			proxyReq.Out.Header.Del("X-API-Key")
			if key := manager.Client().InferenceAPIKey(); key != "" {
				proxyReq.Out.Header.Set("Authorization", "Bearer "+key)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			shared.SendError(w, r, provider.Error{
				Provider: manager.Name(),
				Code:     "provider_inference_failed",
				Message:  "provider inference request failed",
				Status:   http.StatusBadGateway,
			})
		},
	}
	return &ProviderModel{id: id, manager: manager, logger: logger, proxy: reverseProxy}, nil
}

func (p *ProviderModel) Logger() *logmon.Monitor { return p.logger }

func (p *ProviderModel) BeginSwap(victims []string) {
	p.manager.BeginSwap(p.id, victims)
}

func (p *ProviderModel) AcquireLease() bool { return p.manager.Acquire(p.id) }

func (p *ProviderModel) ReleaseLease() { p.manager.Release(p.id) }

func (p *ProviderModel) QueueDelta(delta int) { p.manager.QueueDelta(p.id, delta) }

func (p *ProviderModel) QueueWait(wait time.Duration) { p.manager.QueueWait(p.id, wait) }

func (p *ProviderModel) ResidentAdmission() { p.manager.ResidentAdmission() }

func (p *ProviderModel) FairnessPromotion() { p.manager.FairnessPromotion() }

func (p *ProviderModel) Run(timeout time.Duration) error {
	return p.EnsureReady(context.Background(), timeout)
}

func (p *ProviderModel) EnsureReady(ctx context.Context, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return p.manager.Load(ctx, p.id)
}

func (p *ProviderModel) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		switch p.manager.State(p.id) {
		case provider.StateReady, provider.StateBusy:
			return nil
		case provider.StateFailed:
			return provider.Error{Provider: p.manager.Name(), Code: "model_load_failed", Message: "provider model failed to load"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *ProviderModel) Stop(timeout time.Duration) error {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return p.manager.Unload(ctx, p.id)
}

func (p *ProviderModel) Shutdown(_ time.Duration) error {
	// Lemonade is a persistent service. llemon-swap relinquishes its leases but
	// does not tear down provider residency merely because the proxy exits.
	return nil
}

func (p *ProviderModel) State() ProcessState {
	switch p.manager.State(p.id) {
	case provider.StateLoading:
		return StateStarting
	case provider.StateReady, provider.StateBusy:
		return StateReady
	case provider.StateDraining:
		// A new Run registers demand before waiting on restoration, allowing
		// the transient model to remain resident for the new workload.
		return StateStopped
	case provider.StateUnloading:
		return StateStopping
	default:
		return StateStopped
	}
}

func (p *ProviderModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if state := p.State(); state != StateReady {
		shared.SendError(w, r, provider.Error{
			Code:    "model_not_ready",
			Message: "provider model is not ready",
			Status:  http.StatusServiceUnavailable,
		})
		return
	}
	// Lemonade accepts both /v1 and /api/v1 routes. Keep all payload fields and
	// streaming semantics intact; only the configured model ID was rewritten
	// earlier by the generic model filter middleware.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
	}
	p.proxy.ServeHTTP(w, r)
}
