package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

type fakeLemonade struct {
	mu              sync.Mutex
	server          *httptest.Server
	models          []string
	loaded          map[string]bool
	pinned          map[string]bool
	loads           map[string]int
	unloads         map[string]int
	capacity        int
	globalUnloads   int
	perModelUnloads int
	delay           time.Duration
	failLoad        map[string]int
	lastAuth        string
}

func newFakeLemonade(t *testing.T, capacity int, models ...string) *fakeLemonade {
	t.Helper()
	fake := &fakeLemonade{
		models:   models,
		loaded:   make(map[string]bool),
		pinned:   make(map[string]bool),
		loads:    make(map[string]int),
		unloads:  make(map[string]int),
		capacity: capacity,
		failLoad: make(map[string]int),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeLemonade) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuth = r.Header.Get("Authorization")
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/health":
		var loaded []ResidentModel
		for _, model := range f.models {
			if f.loaded[model] {
				loaded = append(loaded, ResidentModel{Name: model, Type: "llm", Status: "ready", Pinned: f.pinned[model]})
			}
		}
		json.NewEncoder(w).Encode(Health{
			Status:          "ok",
			Version:         "10.3.0",
			AllModelsLoaded: loaded,
			MaxModels:       map[string]int{"llm": f.capacity},
		})
	case "/api/v1/models":
		var data []map[string]string
		for _, model := range f.models {
			data = append(data, map[string]string{"id": model, "owned_by": "lemonade"})
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	case "/api/v1/load":
		var request struct {
			Model  string `json:"model_name"`
			Pinned *bool  `json:"pinned"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		if f.delay > 0 {
			f.mu.Unlock()
			time.Sleep(f.delay)
			f.mu.Lock()
		}
		f.loads[request.Model]++
		if f.failLoad[request.Model] > 0 {
			f.failLoad[request.Model]--
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"code": "load_failed", "message": "injected load failure",
			}})
			return
		}
		if !f.loaded[request.Model] && f.loadedCount() >= f.capacity {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"code": "slots_pinned_error", "message": "all model slots are pinned",
			}})
			return
		}
		f.loaded[request.Model] = true
		if request.Pinned != nil {
			f.pinned[request.Model] = *request.Pinned
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	case "/api/v1/unload":
		var request struct {
			Model string `json:"model_name"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		if request.Model == "" {
			// global unload: evict every loaded model in one call
			f.globalUnloads++
			for model := range f.loaded {
				delete(f.loaded, model)
				delete(f.pinned, model)
				f.unloads[model]++
			}
		} else {
			f.perModelUnloads++
			delete(f.loaded, request.Model)
			delete(f.pinned, request.Model)
			f.unloads[request.Model]++
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLemonade) loadedCount() int {
	count := 0
	for _, loaded := range f.loaded {
		if loaded {
			count++
		}
	}
	return count
}

func providerTestConfig(fake *fakeLemonade, models map[string]config.ModelConfig) config.Config {
	return config.Config{
		Providers: map[string]config.ProviderConfig{
			"local": {
				Type:                config.ProviderTypeLemonade,
				BaseURL:             fake.server.URL,
				ManagementTimeout:   time.Second,
				ColdStartTimeout:    time.Second,
				DiscoveryInterval:   time.Hour,
				Required:            true,
				ResolvedAPIKey:      "inference-secret",
				ResolvedAdminAPIKey: "admin-secret",
			},
		},
		LifecyclePools: map[string]config.LifecyclePoolConfig{
			"primary": {
				Provider:         "local",
				Capacity:         fake.capacity,
				RestorePreferred: true,
				TransientIdleTTL: time.Millisecond,
				ResidentFirst:    true,
				MaxResidentBurst: 8,
				MaxResidentWait:  time.Second,
			},
		},
		Models: models,
	}
}

func lemonadeModel(providerModel, residency string, priority int) config.ModelConfig {
	return config.ModelConfig{
		Provider:          "local",
		ProviderModel:     providerModel,
		LifecyclePool:     "primary",
		Residency:         residency,
		ResidencyPriority: priority,
	}
}

func TestManager_ResidentLLMAliasesExcludesNonLLMModels(t *testing.T) {
	manager := &Manager{
		bindings: map[string]modelBinding{
			"summary": {Config: lemonadeModel("summary-provider", config.ResidencyPreferred, 0)},
			"whisper": {Config: lemonadeModel("whisper-provider", config.ResidencyPreferred, 1)},
			"cold":    {Config: lemonadeModel("cold-provider", config.ResidencyTransient, 2)},
		},
		states: map[string]ModelState{
			"summary": StateReady,
			"whisper": StateReady,
			"cold":    StateUnloaded,
		},
		health: Health{AllModelsLoaded: []ResidentModel{
			{Name: "summary-provider", Type: "llm", Status: "ready"},
			{Name: "whisper-provider", Type: "asr", Status: "ready"},
			{Name: "cold-provider", Type: "llm", Status: "ready"},
		}},
	}

	got := manager.ResidentLLMAliases("primary")
	if len(got) != 1 || got[0] != "summary" {
		t.Fatalf("ResidentLLMAliases() = %v, want [summary]", got)
	}
}

func TestLemonade_DiscoveryAndAuthentication(t *testing.T) {
	fake := newFakeLemonade(t, 2, "one", "two")
	client := NewLemonade("local", providerTestConfig(fake, nil).Providers["local"])
	models, err := client.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "one" || ids[1] != "two" {
		t.Fatalf("models = %v", ids)
	}
	fake.mu.Lock()
	auth := fake.lastAuth
	fake.mu.Unlock()
	if auth != "Bearer admin-secret" {
		t.Fatalf("management Authorization = %q", auth)
	}
}

func TestManager_ConcurrentLoadCoalesces(t *testing.T) {
	fake := newFakeLemonade(t, 1, "cold")
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"cold": lemonadeModel("cold", config.ResidencyTransient, 0),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("cold")

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- manager.Load(context.Background(), "cold")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
	}
	fake.mu.Lock()
	loads := fake.loads["cold"]
	fake.mu.Unlock()
	if loads != 1 {
		t.Fatalf("load calls = %d, want 1", loads)
	}
}

func TestManager_RestoresDisplacedPreferredAndPin(t *testing.T) {
	fake := newFakeLemonade(t, 1, "default", "transient")
	fake.loaded["default"] = true
	fake.pinned["default"] = true
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"default":   lemonadeModel("default", config.ResidencyPreferred, 0),
		"transient": lemonadeModel("transient", config.ResidencyTransient, 10),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("transient")

	manager.BeginSwap("transient", []string{"default"})
	if err := manager.Unload(context.Background(), "default"); err != nil {
		t.Fatalf("Unload preferred: %v", err)
	}
	if err := manager.Load(context.Background(), "transient"); err != nil {
		t.Fatalf("Load transient: %v", err)
	}
	manager.Acquire("transient")
	manager.Release("transient")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		restored := fake.loaded["default"] && fake.pinned["default"] && !fake.loaded["transient"]
		fake.mu.Unlock()
		if restored {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("preferred model was not restored and repinned")
}

func TestManager_ExternalResidencyIsNotUnloaded(t *testing.T) {
	fake := newFakeLemonade(t, 1, "external")
	fake.loaded["external"] = true
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"external": lemonadeModel("external", config.ResidencyExternal, 0),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("external")
	if err := manager.Unload(context.Background(), "external"); err == nil {
		t.Fatal("expected externally owned unload to fail")
	}
}

func TestManager_UnloadAll(t *testing.T) {
	fake := newFakeLemonade(t, 4, "default", "transient", "external")
	fake.loaded["default"] = true
	fake.pinned["default"] = true
	fake.loaded["transient"] = true
	fake.loaded["external"] = true // externally owned, must also be cleared by the global unload
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"default":   lemonadeModel("default", config.ResidencyPreferred, 0),
		"transient": lemonadeModel("transient", config.ResidencyTransient, 10),
		"external":  lemonadeModel("external", config.ResidencyExternal, 0),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("default")

	if err := manager.UnloadAll(context.Background()); err != nil {
		t.Fatalf("UnloadAll: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.globalUnloads != 1 || fake.perModelUnloads != 0 {
		t.Fatalf("UnloadAll should use one global unload (global=%d perModel=%d)", fake.globalUnloads, fake.perModelUnloads)
	}
	if fake.loaded["default"] || fake.loaded["transient"] || fake.loaded["external"] {
		t.Fatalf("UnloadAll should clear every loaded model, got %v", fake.loaded)
	}
	if fake.unloads["default"] != 1 || fake.unloads["transient"] != 1 || fake.unloads["external"] != 1 {
		t.Fatalf("global unload should evict each model once, got %v", fake.unloads)
	}
	for _, alias := range []string{"default", "transient", "external"} {
		if manager.State(alias) != StateUnloaded {
			t.Errorf("State(%s)=%q, want unloaded", alias, manager.State(alias))
		}
		if manager.owned[alias] {
			t.Errorf("owned[%s]=true, want false after UnloadAll", alias)
		}
	}
}

func TestLemonade_NoCapacityReturnsStableCode(t *testing.T) {
	fake := newFakeLemonade(t, 1, "pinned", "cold")
	fake.loaded["pinned"] = true
	fake.pinned["pinned"] = true
	client := NewLemonade("local", providerTestConfig(fake, nil).Providers["local"])
	err := client.Load(context.Background(), "cold", nil)
	providerErr, ok := err.(Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if providerErr.Code != "slots_pinned_error" || providerErr.StatusCode() != http.StatusConflict {
		t.Fatalf("error = %#v", providerErr)
	}
}

func TestManager_RestorationRetriesWithBoundedBackoff(t *testing.T) {
	fake := newFakeLemonade(t, 1, "default", "transient")
	fake.loaded["default"] = true
	fake.pinned["default"] = true
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"default":   lemonadeModel("default", config.ResidencyPreferred, 0),
		"transient": lemonadeModel("transient", config.ResidencyTransient, 10),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("transient")

	manager.BeginSwap("transient", []string{"default"})
	if err := manager.Unload(context.Background(), "default"); err != nil {
		t.Fatalf("Unload preferred: %v", err)
	}
	if err := manager.Load(context.Background(), "transient"); err != nil {
		t.Fatalf("Load transient: %v", err)
	}
	fake.mu.Lock()
	baselineLoads := fake.loads["default"]
	fake.failLoad["default"] = 2
	fake.mu.Unlock()
	manager.Acquire("transient")
	manager.Release("transient")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		restored := fake.loaded["default"] && fake.loads["default"] == baselineLoads+3
		fake.mu.Unlock()
		if restored {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("preferred restoration did not recover after bounded retries")
}

func TestManager_ReconcilesExternalResidencyDrift(t *testing.T) {
	fake := newFakeLemonade(t, 1, "drifting")
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"drifting": lemonadeModel("drifting", config.ResidencyTransient, 0),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("drifting")

	fake.mu.Lock()
	fake.loaded["drifting"] = true
	fake.mu.Unlock()
	if err := manager.reconcile(context.Background(), false); err != nil {
		t.Fatalf("reconcile load drift: %v", err)
	}
	if got := manager.State("drifting"); got != StateReady {
		t.Fatalf("state after external load = %s", got)
	}

	fake.mu.Lock()
	delete(fake.loaded, "drifting")
	fake.mu.Unlock()
	if err := manager.reconcile(context.Background(), false); err != nil {
		t.Fatalf("reconcile unload drift: %v", err)
	}
	if got := manager.State("drifting"); got != StateUnloaded {
		t.Fatalf("state after external unload = %s", got)
	}
	if got := manager.Status().ReconcileCorrections; got < 2 {
		t.Fatalf("reconciliation corrections = %d, want at least 2", got)
	}
}

func TestManager_NewDemandPostponesRestoration(t *testing.T) {
	fake := newFakeLemonade(t, 1, "default", "transient")
	fake.loaded["default"] = true
	fake.pinned["default"] = true
	cfg := providerTestConfig(fake, map[string]config.ModelConfig{
		"default":   lemonadeModel("default", config.ResidencyPreferred, 0),
		"transient": lemonadeModel("transient", config.ResidencyTransient, 10),
	})
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	manager, _ := registry.ManagerForModel("transient")

	manager.BeginSwap("transient", []string{"default"})
	if err := manager.Unload(context.Background(), "default"); err != nil {
		t.Fatalf("Unload preferred: %v", err)
	}
	if err := manager.Load(context.Background(), "transient"); err != nil {
		t.Fatalf("Load transient: %v", err)
	}
	fake.mu.Lock()
	baselineLoads := fake.loads["default"]
	fake.failLoad["default"] = 1
	fake.mu.Unlock()
	if !manager.Acquire("transient") {
		t.Fatal("Acquire transient")
	}
	manager.Release("transient")

	deadline := time.Now().Add(time.Second)
	for {
		fake.mu.Lock()
		restoreStarted := fake.loads["default"] > baselineLoads
		fake.mu.Unlock()
		if restoreStarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restoration did not start")
		}
		time.Sleep(time.Millisecond)
	}

	if err := manager.Load(context.Background(), "transient"); err != nil {
		t.Fatalf("Load new transient demand: %v", err)
	}
	fake.mu.Lock()
	transientReady := fake.loaded["transient"]
	defaultReady := fake.loaded["default"]
	fake.mu.Unlock()
	if !transientReady || defaultReady {
		t.Fatalf("postponed residency: transient=%v default=%v", transientReady, defaultReady)
	}
}

func TestRegistry_IndependentProvidersDoNotBlockEachOther(t *testing.T) {
	slow := newFakeLemonade(t, 1, "slow")
	fast := newFakeLemonade(t, 1, "fast")
	slow.delay = 200 * time.Millisecond
	cfg := config.Config{
		Providers: map[string]config.ProviderConfig{
			"slow": providerTestConfig(slow, nil).Providers["local"],
			"fast": providerTestConfig(fast, nil).Providers["local"],
		},
		LifecyclePools: map[string]config.LifecyclePoolConfig{
			"slow-pool": {Provider: "slow", Capacity: 1},
			"fast-pool": {Provider: "fast", Capacity: 1},
		},
		Models: map[string]config.ModelConfig{
			"slow": {
				Provider: "slow", ProviderModel: "slow", LifecyclePool: "slow-pool", Residency: config.ResidencyTransient,
			},
			"fast": {
				Provider: "fast", ProviderModel: "fast", LifecyclePool: "fast-pool", Residency: config.ResidencyTransient,
			},
		},
	}
	registry, err := NewRegistry(context.Background(), cfg, logmon.New())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(registry.Close)
	slowManager, _ := registry.ManagerForModel("slow")
	fastManager, _ := registry.ManagerForModel("fast")

	slowDone := make(chan error, 1)
	go func() { slowDone <- slowManager.Load(context.Background(), "slow") }()
	time.Sleep(20 * time.Millisecond)
	started := time.Now()
	if err := fastManager.Load(context.Background(), "fast"); err != nil {
		t.Fatalf("fast provider load: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("fast provider blocked for %s", elapsed)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow provider load: %v", err)
	}
}
