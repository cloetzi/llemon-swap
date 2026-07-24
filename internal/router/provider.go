package router

import (
	"fmt"
	"sort"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/provider"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
)

func newConfiguredProcess(
	base *baseRouter,
	id string,
	modelCfg config.ModelConfig,
	registry *provider.Registry,
	procLog, proxyLog *logmon.Monitor,
) (process.Process, error) {
	if modelCfg.Provider == "" {
		return process.New(base.procCtx, id, modelCfg, procLog, proxyLog)
	}
	if registry == nil {
		return nil, fmt.Errorf("provider registry is required for model %q", id)
	}
	manager, ok := registry.ManagerForModel(id)
	if !ok {
		return nil, fmt.Errorf("provider manager not found for model %q", id)
	}
	return process.NewProviderModel(id, manager, procLog)
}

// lifecycleSwapper overlays provider-pool capacity on the selected upstream
// process swapper. Existing group/matrix semantics remain unchanged for
// process-managed models.
type lifecycleSwapper struct {
	cfg      config.Config
	registry *provider.Registry
	fallback scheduler.Swapper
	logger   *logmon.Monitor
}

func wrapLifecycleSwapper(cfg config.Config, registry *provider.Registry, fallback scheduler.Swapper, logger *logmon.Monitor) scheduler.Swapper {
	if len(cfg.Providers) == 0 {
		return fallback
	}
	return &lifecycleSwapper{cfg: cfg, registry: registry, fallback: fallback, logger: logger}
}

func (s *lifecycleSwapper) EvictionFor(target string, running []string) []string {
	targetCfg, ok := s.cfg.Models[target]
	if !ok || targetCfg.Provider == "" {
		filtered := make([]string, 0, len(running))
		for _, id := range running {
			if s.cfg.Models[id].Provider == "" {
				filtered = append(filtered, id)
			}
		}
		return s.fallback.EvictionFor(target, filtered)
	}
	manager, ok := s.registry.ManagerForModel(target)
	if !ok {
		return nil
	}
	pool := s.cfg.LifecyclePools[targetCfg.LifecyclePool]
	residents := manager.ResidentAliases(targetCfg.LifecyclePool)
	for _, id := range residents {
		if id == target {
			return nil
		}
	}
	occupied := manager.OccupiedLLM()
	if occupied < pool.Capacity {
		return nil
	}

	type candidate struct {
		id        string
		residency string
		priority  int
	}
	var candidates []candidate
	for _, id := range residents {
		modelCfg := s.cfg.Models[id]
		if modelCfg.Residency == config.ResidencyHardPinned || modelCfg.Residency == config.ResidencyExternal {
			continue
		}
		candidates = append(candidates, candidate{id: id, residency: modelCfg.Residency, priority: modelCfg.ResidencyPriority})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].residency != candidates[j].residency {
			return candidates[i].residency == config.ResidencyTransient
		}
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].id > candidates[j].id
	})
	need := occupied - pool.Capacity + 1
	if need > len(candidates) {
		// The provider's remaining slots are hard-pinned or externally owned.
		// Let Load return the stable no_evictable_capacity error without first
		// performing a destructive partial transition.
		return nil
	}
	evict := make([]string, 0, need)
	for i := 0; i < need; i++ {
		evict = append(evict, candidates[i].id)
	}
	return evict
}

func (s *lifecycleSwapper) OnSwapStart(target string, running []string) {
	if modelCfg, ok := s.cfg.Models[target]; !ok || modelCfg.Provider == "" {
		s.fallback.OnSwapStart(target, running)
		return
	}
	evict := s.EvictionFor(target, running)
	if len(evict) > 0 {
		s.logger.Infof("provider=%s model=%s transition=swap evict=%v", s.cfg.Models[target].Provider, target, evict)
	}
}
