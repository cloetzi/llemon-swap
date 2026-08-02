package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/provider"
)

func TestLifecycleSwapper_ProcessTargetUsesProviderVictims(t *testing.T) {
	planner := &stubPlanner{evict: map[string][]string{
		"ds4": {"embed", "summary"},
	}}
	swapper := &lifecycleSwapper{
		cfg: config.Config{Models: map[string]config.ModelConfig{
			"ds4":     {},
			"embed":   {Provider: "lemonade"},
			"summary": {Provider: "lemonade"},
		}},
		fallback: planner,
	}

	got := swapper.EvictionFor("ds4", []string{"embed", "summary"})
	if len(got) != 2 || got[0] != "embed" || got[1] != "summary" {
		t.Fatalf("EvictionFor() = %v, want [embed summary]", got)
	}
}

func TestLifecycleSwapper_ProviderTargetCombinesMatrixAndCapacity(t *testing.T) {
	residents := []provider.ResidentModel{
		{Name: "summary-provider", Type: "llm", Status: "ready"},
		{Name: "qwen-provider", Type: "llm", Status: "ready"},
		{Name: "gemma-provider", Type: "llm", Status: "ready"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			json.NewEncoder(w).Encode(provider.Health{
				Status:          "ok",
				AllModelsLoaded: residents,
				MaxModels:       map[string]int{"llm": 4},
			})
		case "/api/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
				{"id": "summary-provider"},
				{"id": "qwen-provider"},
				{"id": "gemma-provider"},
				{"id": "large-provider"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	models := map[string]config.ModelConfig{
		"summary": providerModelConfig("summary-provider", config.ResidencyPreferred),
		"qwen":    providerModelConfig("qwen-provider", config.ResidencyTransient),
		"gemma":   providerModelConfig("gemma-provider", config.ResidencyTransient),
		"large":   providerModelConfig("large-provider", config.ResidencyTransient),
	}
	cfg := config.Config{
		Models: models,
		Providers: map[string]config.ProviderConfig{
			"lemonade": {
				BaseURL:           server.URL,
				ManagementTimeout: time.Second,
				ColdStartTimeout:  time.Second,
				DiscoveryInterval: time.Hour,
				Required:          true,
			},
		},
		LifecyclePools: map[string]config.LifecyclePoolConfig{
			"primary": {Provider: "lemonade", Capacity: 4},
		},
	}
	registry, err := provider.NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)

	matrixCfg := &config.MatrixConfig{
		Sets: config.OrderedSets{
			{Name: "standard", DSL: "summary & qwen & gemma"},
			{Name: "standalone", DSL: "summary & large"},
		},
	}
	if err := config.ValidateMatrix(matrixCfg, models); err != nil {
		t.Fatalf("ValidateMatrix: %v", err)
	}
	matrix := &matrixSwapper{solver: newMatrixSolver(matrixCfg.Program(), matrixCfg.ResolvedEvictCosts()), logger: logmon.New()}
	swapper := wrapLifecycleSwapper(cfg, registry, matrix, logmon.New())

	got := swapper.EvictionFor("large", []string{"summary", "qwen", "gemma"})
	if len(got) != 2 || got[0] != "qwen" || got[1] != "gemma" {
		t.Fatalf("EvictionFor() = %v, want [qwen gemma]", got)
	}
}

func providerModelConfig(providerModel, residency string) config.ModelConfig {
	return config.ModelConfig{
		Provider:      "lemonade",
		ProviderModel: providerModel,
		LifecyclePool: "primary",
		Residency:     residency,
	}
}

// TestMatrix_GlobalUnloadWhenProviderFullyEvicted verifies that swapping to a
// process-managed target which requires a completely empty provider (the matrix
// set contains no lemonade model, so every configured model is evicted) uses a
// single global unload instead of one unload per model. It also confirms the
// global endpoint clears externally owned models that per-model unloads refuse
// to touch.
func TestMatrix_GlobalUnloadWhenProviderFullyEvicted(t *testing.T) {
	fake := newRouterFakeLemonade(t, 3, "default", "transient", "external")
	fake.mu.Lock()
	fake.loaded["default"] = true
	fake.loaded["transient"] = true
	fake.loaded["external"] = true
	fake.mu.Unlock()

	models := map[string]config.ModelConfig{
		"default":   providerModelConfig("default", config.ResidencyPreferred),
		"transient": providerModelConfig("transient", config.ResidencyTransient),
		"external":  providerModelConfig("external", config.ResidencyExternal),
		"ds4":       {}, // process-managed target requiring an empty provider
	}
	cfg := config.Config{
		HealthCheckTimeout: 5,
		Models:             models,
		Providers: map[string]config.ProviderConfig{
			"lemonade": {
				BaseURL:           fake.server.URL,
				ManagementTimeout: time.Second,
				ColdStartTimeout:  time.Second,
				DiscoveryInterval: time.Hour,
				Required:          true,
			},
		},
		LifecyclePools: map[string]config.LifecyclePoolConfig{
			"primary": {Provider: "lemonade", Capacity: 3},
		},
	}
	registry, err := provider.NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, ok := registry.ManagerForModel("default")
	if !ok {
		t.Fatalf("no manager for default")
	}
	procLog := logmon.NewWriter(io.Discard)
	proxyLog := logmon.NewWriter(io.Discard)
	processes := map[string]process.Process{
		"default":   mustProviderModel(t, "default", manager, procLog),
		"transient": mustProviderModel(t, "transient", manager, procLog),
		"external":  mustProviderModel(t, "external", manager, procLog),
	}
	ds4 := newFakeProcess("ds4")
	ds4.autoReady = true
	processes["ds4"] = ds4

	matrixCfg := &config.MatrixConfig{Sets: config.OrderedSets{{Name: "d", DSL: "ds4"}}}
	if err := config.ValidateMatrix(matrixCfg, models); err != nil {
		t.Fatalf("ValidateMatrix: %v", err)
	}
	matrix := &matrixSwapper{solver: newMatrixSolver(matrixCfg.Program(), matrixCfg.ResolvedEvictCosts()), logger: logmon.New()}
	swapper := wrapLifecycleSwapper(cfg, registry, matrix, logmon.New())
	base, err := newBaseRouter("matrix", cfg, processes, proxyLog, swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	r := &Matrix{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !r.shuttingDown.Load() {
			_ = r.Shutdown(time.Second)
		}
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("ds4"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		g := fake.global
		p := fake.perModel
		fake.mu.Unlock()
		if g > 0 {
			if g != 1 || p != 0 {
				t.Fatalf("swap should use one global unload (global=%d perModel=%d)", g, p)
			}
			if ds4.State() != process.StateReady {
				t.Fatalf("ds4 should be ready after swap, got %q", ds4.State())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	fake.mu.Lock()
	g, p := fake.global, fake.perModel
	fake.mu.Unlock()
	t.Fatalf("global unload not observed (global=%d perModel=%d code=%d)", g, p, w.Code)
}

// TestMatrix_PerModelUnloadWhenProviderNotFullyEvicted verifies the fallback:
// when the swap does not empty the whole provider (a lemonade model stays
// resident), victims are unloaded one at a time rather than via a global
// unload.
func TestMatrix_PerModelUnloadWhenProviderNotFullyEvicted(t *testing.T) {
	fake := newRouterFakeLemonade(t, 2, "default", "cold", "transient")
	fake.mu.Lock()
	fake.loaded["default"] = true
	fake.loaded["cold"] = true
	fake.mu.Unlock()

	models := map[string]config.ModelConfig{
		"default":   providerModelConfig("default", config.ResidencyPreferred),
		"cold":      providerModelConfig("cold", config.ResidencyTransient),
		"transient": providerModelConfig("transient", config.ResidencyTransient),
	}
	cfg := config.Config{
		HealthCheckTimeout: 5,
		Models:             models,
		Providers: map[string]config.ProviderConfig{
			"lemonade": {
				BaseURL:           fake.server.URL,
				ManagementTimeout: time.Second,
				ColdStartTimeout:  time.Second,
				DiscoveryInterval: time.Hour,
				Required:          true,
			},
		},
		LifecyclePools: map[string]config.LifecyclePoolConfig{
			"primary": {Provider: "lemonade", Capacity: 2},
		},
	}
	registry, err := provider.NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, ok := registry.ManagerForModel("default")
	if !ok {
		t.Fatalf("no manager for default")
	}
	procLog := logmon.NewWriter(io.Discard)
	proxyLog := logmon.NewWriter(io.Discard)
	processes := map[string]process.Process{
		"default":   mustProviderModel(t, "default", manager, procLog),
		"cold":      mustProviderModel(t, "cold", manager, procLog),
		"transient": mustProviderModel(t, "transient", manager, procLog),
	}

	matrixCfg := &config.MatrixConfig{Sets: config.OrderedSets{{Name: "s1", DSL: "default & transient"}}}
	if err := config.ValidateMatrix(matrixCfg, models); err != nil {
		t.Fatalf("ValidateMatrix: %v", err)
	}
	matrix := &matrixSwapper{solver: newMatrixSolver(matrixCfg.Program(), matrixCfg.ResolvedEvictCosts()), logger: logmon.New()}
	swapper := wrapLifecycleSwapper(cfg, registry, matrix, logmon.New())
	base, err := newBaseRouter("matrix", cfg, processes, proxyLog, swapper)
	if err != nil {
		t.Fatalf("newBaseRouter: %v", err)
	}
	base.testProcessed = make(chan struct{}, 64)
	r := &Matrix{baseRouter: base}
	go base.run()
	t.Cleanup(func() {
		if !r.shuttingDown.Load() {
			_ = r.Shutdown(time.Second)
		}
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("transient"))

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		g := fake.global
		p := fake.perModel
		fake.mu.Unlock()
		if p > 0 {
			if g != 0 || p != 1 {
				t.Fatalf("partial swap should unload per-model (global=%d perModel=%d)", g, p)
			}
			if manager.State("cold") != provider.StateUnloaded {
				t.Fatalf("cold should be unloaded, got %q", manager.State("cold"))
			}
			if manager.State("transient") != provider.StateReady {
				t.Fatalf("transient should be ready, got %q", manager.State("transient"))
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	fake.mu.Lock()
	g, p := fake.global, fake.perModel
	fake.mu.Unlock()
	t.Fatalf("per-model unload not observed (global=%d perModel=%d code=%d)", g, p, w.Code)
}

// routerFakeLemonade is a minimal Lemonade-compatible HTTP server used by
// router-level tests to observe global versus per-model unloads.
type routerFakeLemonade struct {
	mu       sync.Mutex
	server   *httptest.Server
	loaded   map[string]bool
	global   int
	perModel int
	capacity int
}

func newRouterFakeLemonade(t *testing.T, capacity int, models ...string) *routerFakeLemonade {
	t.Helper()
	fake := &routerFakeLemonade{loaded: map[string]bool{}, capacity: capacity}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			var loaded []provider.ResidentModel
			for _, m := range models {
				if fake.loaded[m] {
					loaded = append(loaded, provider.ResidentModel{Name: m, Type: "llm", Status: "ready"})
				}
			}
			json.NewEncoder(w).Encode(provider.Health{
				Status: "ok", Version: "10.3.0", AllModelsLoaded: loaded, MaxModels: map[string]int{"llm": capacity},
			})
		case "/api/v1/models":
			var data []map[string]string
			for _, m := range models {
				data = append(data, map[string]string{"id": m, "owned_by": "lemonade"})
			}
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case "/api/v1/load":
			var req struct {
				Model string `json:"model_name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			fake.loaded[req.Model] = true
			json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		case "/api/v1/unload":
			var req struct {
				Model string `json:"model_name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Model == "" {
				fake.global++
				for m := range fake.loaded {
					delete(fake.loaded, m)
				}
			} else {
				fake.perModel++
				delete(fake.loaded, req.Model)
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func mustProviderModel(t *testing.T, id string, manager *provider.Manager, logger *logmon.Monitor) process.Process {
	t.Helper()
	p, err := process.NewProviderModel(id, manager, logger)
	if err != nil {
		t.Fatalf("NewProviderModel(%s): %v", id, err)
	}
	return p
}
