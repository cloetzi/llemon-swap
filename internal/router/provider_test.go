package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
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

	matrix := &matrixSwapper{solver: newMatrixSolver([]config.ExpandedSet{
		{SetName: "standard", Models: []string{"summary", "qwen", "gemma"}},
		{SetName: "standalone", Models: []string{"summary", "large"}},
	}, nil), logger: logmon.New()}
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
