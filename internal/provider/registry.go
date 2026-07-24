package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

type modelBinding struct {
	Config config.ModelConfig
}

type displacedModel struct {
	Alias     string
	Model     string
	WasPinned bool
	Priority  int
}

// Manager serializes lifecycle transitions for one provider. Inference never
// takes opMu; only health/load/unload/pin transitions are provider-wide.
type Manager struct {
	name   string
	client Lifecycle
	cfg    config.ProviderConfig
	log    *logmon.Monitor

	opMu sync.Mutex
	mu   sync.RWMutex

	bindings         map[string]modelBinding
	pools            map[string]config.LifecyclePoolConfig
	health           Health
	discovered       []string
	healthy          bool
	lastError        string
	reconciledAt     time.Time
	states           map[string]ModelState
	owned            map[string]bool
	active           map[string]int
	queued           map[string]int
	demand           map[string]int
	transitions      map[string]string
	displaced        map[string][]displacedModel
	timers           map[string]*time.Timer
	corrections      atomic.Uint64
	coalesced        atomic.Uint64
	failures         atomic.Uint64
	residentFirst    atomic.Uint64
	fairPromotions   atomic.Uint64
	loadDurations    map[string]float64
	unloadDurations  map[string]float64
	restoreDurations map[string]float64
	queueWaits       map[string]float64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

type Registry struct {
	managers map[string]*Manager
	models   map[string]*Manager
}

func NewRegistry(ctx context.Context, cfg config.Config, log *logmon.Monitor) (*Registry, error) {
	registry := &Registry{
		managers: make(map[string]*Manager, len(cfg.Providers)),
		models:   make(map[string]*Manager),
	}
	for name, providerCfg := range cfg.Providers {
		managerCtx, cancel := context.WithCancel(ctx)
		manager := &Manager{
			name:             name,
			client:           NewLemonade(name, providerCfg),
			cfg:              providerCfg,
			log:              log,
			bindings:         make(map[string]modelBinding),
			pools:            make(map[string]config.LifecyclePoolConfig),
			states:           make(map[string]ModelState),
			owned:            make(map[string]bool),
			active:           make(map[string]int),
			queued:           make(map[string]int),
			demand:           make(map[string]int),
			transitions:      make(map[string]string),
			displaced:        make(map[string][]displacedModel),
			timers:           make(map[string]*time.Timer),
			loadDurations:    make(map[string]float64),
			unloadDurations:  make(map[string]float64),
			restoreDurations: make(map[string]float64),
			queueWaits:       make(map[string]float64),
			ctx:              managerCtx,
			cancel:           cancel,
			done:             make(chan struct{}),
		}
		for poolName, pool := range cfg.LifecyclePools {
			if pool.Provider == name {
				manager.pools[poolName] = pool
			}
		}
		for alias, modelCfg := range cfg.Models {
			if modelCfg.Provider != name {
				continue
			}
			manager.bindings[alias] = modelBinding{Config: modelCfg}
			manager.states[alias] = StateUnloaded
			registry.models[alias] = manager
		}

		if err := manager.reconcile(ctx, true); err != nil {
			if providerCfg.Required {
				cancel()
				registry.Close()
				return nil, fmt.Errorf("connecting provider %s: %w", name, err)
			}
			log.Warnf("provider=%s optional provider unavailable: %v", name, err)
		}
		if err := manager.validateCapacity(); err != nil {
			cancel()
			registry.Close()
			return nil, err
		}
		registry.managers[name] = manager
		go manager.watch()
	}
	return registry, nil
}

func (r *Registry) ManagerForModel(alias string) (*Manager, bool) {
	m, ok := r.models[alias]
	return m, ok
}

func (r *Registry) Manager(name string) (*Manager, bool) {
	m, ok := r.managers[name]
	return m, ok
}

func (r *Registry) Close() {
	for _, manager := range r.managers {
		manager.cancel()
	}
	for _, manager := range r.managers {
		<-manager.done
	}
}

func (r *Registry) Status() []Status {
	statuses := make([]Status, 0, len(r.managers))
	for _, manager := range r.managers {
		statuses = append(statuses, manager.Status())
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func (m *Manager) Client() Lifecycle { return m.client }

func (m *Manager) Name() string { return m.name }

func (m *Manager) Binding(alias string) (config.ModelConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	binding, ok := m.bindings[alias]
	return binding.Config, ok
}

func (m *Manager) State(alias string) ModelState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.states[alias]
}

func (m *Manager) ResidentAliases(pool string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var aliases []string
	for alias, binding := range m.bindings {
		if binding.Config.LifecyclePool == pool && m.states[alias] != StateUnloaded && m.states[alias] != StateFailed {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	return aliases
}

// OccupiedLLM reports all observed Lemonade LLM residency, including models
// not configured in llemon-swap. Destructive planning uses this fresh
// cooperative-provider view so externally owned slots are never invisible.
func (m *Manager) OccupiedLLM() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	occupied := 0
	for _, resident := range m.health.AllModelsLoaded {
		if resident.Type == "" || resident.Type == "llm" {
			occupied++
		}
	}
	return occupied
}

func (m *Manager) BeginSwap(target string, victims []string) {
	targetCfg, ok := m.Binding(target)
	if !ok || targetCfg.Residency != config.ResidencyTransient {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool)
	for _, existing := range m.displaced[target] {
		seen[existing.Alias] = true
	}
	for _, victim := range victims {
		binding, ok := m.bindings[victim]
		if !ok || binding.Config.Residency != config.ResidencyPreferred || seen[victim] {
			continue
		}
		displaced := displacedModel{
			Alias:    victim,
			Model:    binding.Config.ProviderModel,
			Priority: binding.Config.ResidencyPriority,
		}
		for _, resident := range m.health.AllModelsLoaded {
			if resident.Name == displaced.Model {
				displaced.WasPinned = resident.Pinned
				break
			}
		}
		m.displaced[target] = append(m.displaced[target], displaced)
		seen[victim] = true
	}
	sort.Slice(m.displaced[target], func(i, j int) bool {
		a, b := m.displaced[target][i], m.displaced[target][j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.Alias < b.Alias
	})
}

func (m *Manager) Load(ctx context.Context, alias string) error {
	binding, ok := m.Binding(alias)
	if !ok {
		return Error{Provider: m.name, Code: "model_not_configured", Message: "model is not configured on provider"}
	}
	m.mu.Lock()
	m.demand[alias]++
	if timer := m.timers[alias]; timer != nil {
		timer.Stop()
		delete(m.timers, alias)
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.demand[alias]--
		m.mu.Unlock()
	}()
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if err := m.reconcileLocked(ctx, false); err != nil {
		return err
	}
	if m.State(alias) == StateReady {
		m.coalesced.Add(1)
		return nil
	}
	if binding.Residency == config.ResidencyExternal {
		return Error{Provider: m.name, Code: "external_model_not_resident", Message: "externally managed model is not resident", Status: 409}
	}
	pool := m.pools[binding.LifecyclePool]
	m.mu.RLock()
	occupied := 0
	for _, resident := range m.health.AllModelsLoaded {
		if resident.Type == "" || resident.Type == "llm" {
			occupied++
		}
	}
	m.mu.RUnlock()
	if occupied >= pool.Capacity {
		return Error{
			Provider: m.name,
			Code:     "no_evictable_capacity",
			Message:  "provider capacity is occupied by pinned or externally owned models",
			Status:   409,
		}
	}

	m.setTransition(alias, string(StateLoading))
	pinned := binding.Residency == config.ResidencyHardPinned || binding.Residency == config.ResidencyPreferred
	started := time.Now()
	err := m.client.Load(ctx, binding.ProviderModel, boolPtr(pinned))
	if err != nil {
		m.setFailed(alias, err)
		return err
	}
	if err := m.reconcileLocked(ctx, false); err != nil {
		m.setFailed(alias, err)
		return err
	}
	if m.State(alias) != StateReady {
		err := Error{Provider: m.name, Code: "model_not_ready", Message: "provider completed load but model is not ready"}
		m.setFailed(alias, err)
		return err
	}
	m.mu.Lock()
	m.owned[alias] = true
	m.loadDurations[alias] = time.Since(started).Seconds()
	delete(m.transitions, alias)
	m.mu.Unlock()
	m.log.Infof("provider=%s model=%s transition=load duration=%s", m.name, alias, time.Since(started).Round(time.Millisecond))
	return nil
}

func (m *Manager) Unload(ctx context.Context, alias string) error {
	binding, ok := m.Binding(alias)
	if !ok {
		return nil
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if err := m.reconcileLocked(ctx, false); err != nil {
		return err
	}
	m.mu.RLock()
	active := m.active[alias]
	owned := m.owned[alias]
	state := m.states[alias]
	m.mu.RUnlock()
	if active > 0 {
		return Error{Provider: m.name, Code: "model_busy", Message: "model still has active requests", Status: 409}
	}
	if state == StateUnloaded {
		return nil
	}
	if binding.Residency == config.ResidencyExternal && !owned {
		return Error{Provider: m.name, Code: "externally_owned", Message: "refusing to unload an externally owned model", Status: 409}
	}

	m.setTransition(alias, string(StateUnloading))
	started := time.Now()
	if binding.Residency == config.ResidencyPreferred {
		if err := m.client.Pin(ctx, binding.ProviderModel, false); err != nil {
			m.setFailed(alias, err)
			return err
		}
	}
	if err := m.client.Unload(ctx, binding.ProviderModel); err != nil {
		m.setFailed(alias, err)
		return err
	}
	if err := m.reconcileLocked(ctx, false); err != nil {
		m.setFailed(alias, err)
		return err
	}
	m.mu.Lock()
	m.owned[alias] = false
	m.states[alias] = StateUnloaded
	m.unloadDurations[alias] = time.Since(started).Seconds()
	delete(m.transitions, alias)
	m.mu.Unlock()
	m.log.Infof("provider=%s model=%s transition=unload duration=%s", m.name, alias, time.Since(started).Round(time.Millisecond))
	return nil
}

func (m *Manager) Acquire(alias string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[alias] != StateReady && m.states[alias] != StateBusy {
		return false
	}
	if timer := m.timers[alias]; timer != nil {
		timer.Stop()
		delete(m.timers, alias)
	}
	m.active[alias]++
	if m.states[alias] == StateReady {
		m.states[alias] = StateBusy
	}
	return true
}

func (m *Manager) Release(alias string) {
	binding, ok := m.Binding(alias)
	if !ok {
		return
	}
	m.mu.Lock()
	if m.active[alias] > 0 {
		m.active[alias]--
	}
	if m.active[alias] == 0 && m.states[alias] == StateBusy {
		m.states[alias] = StateReady
	}
	pool := m.pools[binding.LifecyclePool]
	shouldRestore := binding.Residency == config.ResidencyTransient && pool.RestorePreferred && len(m.displaced[alias]) > 0 && m.active[alias] == 0
	if shouldRestore {
		ttl := pool.TransientIdleTTL
		m.timers[alias] = time.AfterFunc(ttl, func() { m.restore(alias) })
	}
	m.mu.Unlock()
}

func (m *Manager) QueueDelta(alias string, delta int) {
	m.mu.Lock()
	m.queued[alias] += delta
	if m.queued[alias] < 0 {
		m.queued[alias] = 0
	}
	m.mu.Unlock()
}

func (m *Manager) QueueWait(alias string, wait time.Duration) {
	m.mu.Lock()
	m.queueWaits[alias] = wait.Seconds()
	m.mu.Unlock()
}

func (m *Manager) ResidentAdmission() { m.residentFirst.Add(1) }

func (m *Manager) FairnessPromotion() { m.fairPromotions.Add(1) }

func (m *Manager) restore(transient string) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	delete(m.timers, transient)
	if m.active[transient] > 0 {
		m.mu.Unlock()
		return
	}
	displaced := append([]displacedModel(nil), m.displaced[transient]...)
	if len(displaced) == 0 {
		m.mu.Unlock()
		return
	}
	m.transitions[transient] = "restoring"
	if m.demand[transient] > 0 {
		delete(m.transitions, transient)
		m.mu.Unlock()
		return
	}
	m.states[transient] = StateDraining
	m.mu.Unlock()

	started := time.Now()
	ctx := m.ctx
	if err := m.reconcileLocked(ctx, false); err != nil {
		m.setFailed(transient, err)
		return
	}
	m.mu.Lock()
	if m.active[transient] > 0 || m.demand[transient] > 0 {
		m.states[transient] = StateReady
		delete(m.transitions, transient)
		m.mu.Unlock()
		return
	}
	m.states[transient] = StateDraining
	m.mu.Unlock()
	binding, _ := m.Binding(transient)
	if m.State(transient) != StateUnloaded {
		if err := m.client.Unload(ctx, binding.ProviderModel); err != nil {
			m.setFailed(transient, err)
			return
		}
		if err := m.reconcileLocked(ctx, false); err != nil {
			m.setFailed(transient, err)
			return
		}
	}

	for _, item := range displaced {
		if m.resumeDemandedTransient(ctx, transient, binding) {
			return
		}
		modelCfg, ok := m.Binding(item.Alias)
		if !ok {
			continue
		}
		pool := m.pools[modelCfg.LifecyclePool]
		m.mu.RLock()
		occupied := 0
		for _, resident := range m.health.AllModelsLoaded {
			if resident.Type == "" || resident.Type == "llm" {
				occupied++
			}
		}
		m.mu.RUnlock()
		if occupied >= pool.Capacity {
			m.setFailed(item.Alias, Error{
				Provider: m.name,
				Code:     "restoration_capacity_blocked",
				Message:  "preferred restoration is blocked by external provider residency",
				Status:   409,
			})
			return
		}
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if m.resumeDemandedTransient(ctx, transient, binding) {
				return
			}
			pinned := item.WasPinned || modelCfg.Residency == config.ResidencyPreferred
			lastErr = m.client.Load(ctx, modelCfg.ProviderModel, boolPtr(pinned))
			if lastErr == nil {
				break
			}
			select {
			case <-time.After(time.Duration(1<<attempt) * 100 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
		if lastErr != nil {
			m.setFailed(item.Alias, lastErr)
			return
		}
		if err := m.reconcileLocked(ctx, false); err != nil {
			m.setFailed(item.Alias, err)
			return
		}
		if m.hasDemand(transient) {
			_ = m.client.Pin(ctx, modelCfg.ProviderModel, false)
			_ = m.client.Unload(ctx, modelCfg.ProviderModel)
			_ = m.reconcileLocked(ctx, false)
			m.resumeDemandedTransient(ctx, transient, binding)
			return
		}
	}
	m.mu.Lock()
	delete(m.displaced, transient)
	delete(m.transitions, transient)
	m.states[transient] = StateUnloaded
	for _, item := range displaced {
		m.owned[item.Alias] = true
	}
	m.restoreDurations[transient] = time.Since(started).Seconds()
	m.mu.Unlock()
	m.log.Infof("provider=%s model=%s transition=restore duration=%s restored=%d", m.name, transient, time.Since(started).Round(time.Millisecond), len(displaced))
}

// resumeDemandedTransient postpones restoration when new work arrived after
// the idle timer fired. It runs with opMu held and preserves the displaced set
// so Release can attempt restoration after the new workload becomes idle.
func (m *Manager) resumeDemandedTransient(ctx context.Context, transient string, binding config.ModelConfig) bool {
	if !m.hasDemand(transient) {
		return false
	}
	if m.State(transient) == StateUnloaded {
		if err := m.client.Load(ctx, binding.ProviderModel, boolPtr(false)); err != nil {
			m.setFailed(transient, err)
			return true
		}
		if err := m.reconcileLocked(ctx, false); err != nil {
			m.setFailed(transient, err)
			return true
		}
	}
	m.mu.Lock()
	m.states[transient] = StateReady
	delete(m.transitions, transient)
	m.mu.Unlock()
	m.log.Infof("provider=%s model=%s transition=restore_postponed reason=new_demand", m.name, transient)
	return true
}

func (m *Manager) hasDemand(alias string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.demand[alias] > 0
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := Status{
		Name:                   m.name,
		Type:                   config.ProviderTypeLemonade,
		Capabilities:           m.client.Capabilities(),
		Healthy:                m.healthy,
		Version:                m.health.Version,
		LastError:              m.lastError,
		LastReconciled:         m.reconciledAt,
		DiscoveredModels:       append([]string(nil), m.discovered...),
		ResidentModels:         append([]ResidentModel(nil), m.health.AllModelsLoaded...),
		Queued:                 cloneIntMap(m.queued),
		Active:                 cloneIntMap(m.active),
		Transitions:            cloneStringMap(m.transitions),
		Restoring:              make(map[string][]string),
		ReconcileCorrections:   m.corrections.Load(),
		LoadDurationSeconds:    cloneFloatMap(m.loadDurations),
		UnloadDurationSeconds:  cloneFloatMap(m.unloadDurations),
		RestoreDurationSeconds: cloneFloatMap(m.restoreDurations),
		QueueWaitSeconds:       cloneFloatMap(m.queueWaits),
		CoalescedLoads:         m.coalesced.Load(),
		FailedTransitions:      m.failures.Load(),
		ResidentAdmissions:     m.residentFirst.Load(),
		FairnessPromotions:     m.fairPromotions.Load(),
	}
	for alias, binding := range m.bindings {
		if binding.Config.Residency == config.ResidencyPreferred || binding.Config.Residency == config.ResidencyHardPinned {
			status.DesiredModels = append(status.DesiredModels, alias)
		}
	}
	for transient, displaced := range m.displaced {
		for _, item := range displaced {
			status.Restoring[transient] = append(status.Restoring[transient], item.Alias)
		}
	}
	sort.Strings(status.DesiredModels)
	return status
}

func (m *Manager) watch() {
	defer close(m.done)
	ticker := time.NewTicker(m.cfg.DiscoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := m.reconcile(m.ctx, true); err != nil {
				m.log.Warnf("provider=%s reconciliation failed: %v", m.name, err)
			}
		case <-m.ctx.Done():
			m.mu.Lock()
			for _, timer := range m.timers {
				timer.Stop()
			}
			m.mu.Unlock()
			return
		}
	}
}

func (m *Manager) reconcile(ctx context.Context, discover bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.reconcileLocked(ctx, discover)
}

func (m *Manager) reconcileLocked(ctx context.Context, discover bool) error {
	health, err := m.client.Health(ctx)
	if err != nil {
		m.mu.Lock()
		m.healthy = false
		m.lastError = err.Error()
		m.mu.Unlock()
		return err
	}
	if err := validateLemonadeHealth(m.name, health); err != nil {
		return err
	}
	var discovered []string
	if discover {
		models, discoverErr := m.client.Discover(ctx)
		if discoverErr != nil {
			m.mu.Lock()
			m.healthy = false
			m.lastError = discoverErr.Error()
			m.mu.Unlock()
			return discoverErr
		}
		for _, model := range models {
			discovered = append(discovered, model.ID)
		}
		sort.Strings(discovered)
	}

	resident := make(map[string]ResidentModel, len(health.AllModelsLoaded))
	for _, model := range health.AllModelsLoaded {
		resident[model.Name] = model
	}
	var corrections uint64
	m.mu.Lock()
	for alias, binding := range m.bindings {
		old := m.states[alias]
		next := StateUnloaded
		if model, ok := resident[binding.Config.ProviderModel]; ok {
			switch model.Status {
			case "loading":
				next = StateLoading
			case "unloading", "evicting":
				next = StateUnloading
			case "failed", "error":
				next = StateFailed
			default:
				if residentReady(model) {
					next = StateReady
				}
			}
		}
		if m.active[alias] > 0 && next == StateReady {
			next = StateBusy
		}
		if old != next && old != "" {
			corrections++
		}
		m.states[alias] = next
	}
	m.health = health
	m.healthy = true
	m.lastError = ""
	m.reconciledAt = time.Now()
	if discover {
		m.discovered = discovered
	}
	m.mu.Unlock()
	if corrections > 0 {
		m.corrections.Add(corrections)
	}
	return nil
}

func (m *Manager) validateCapacity() error {
	m.mu.RLock()
	maxLLM, reported := m.health.MaxModels["llm"]
	m.mu.RUnlock()
	if !reported || maxLLM < 0 {
		return nil
	}
	for name, pool := range m.pools {
		if pool.Capacity > maxLLM {
			return fmt.Errorf("lifecyclePools.%s.capacity=%d exceeds provider %s observed llm capacity=%d", name, pool.Capacity, m.name, maxLLM)
		}
	}
	return nil
}

func (m *Manager) setTransition(alias, transition string) {
	m.mu.Lock()
	m.transitions[alias] = transition
	switch transition {
	case string(StateLoading):
		m.states[alias] = StateLoading
	case string(StateUnloading):
		m.states[alias] = StateUnloading
	}
	m.mu.Unlock()
}

func (m *Manager) setFailed(alias string, err error) {
	m.failures.Add(1)
	m.mu.Lock()
	m.states[alias] = StateFailed
	m.transitions[alias] = "failed/backoff"
	m.lastError = err.Error()
	m.mu.Unlock()
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for key, value := range in {
		if value > 0 {
			out[key] = value
		}
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
