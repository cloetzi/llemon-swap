package config

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func LoadConfigFromReader(r io.Reader) (Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Config{}, err
	}
	yamlStr := string(data)

	// Phase 1: Substitute all ${env.VAR} macros at string level
	// This is safe because env values are simple strings without YAML formatting
	yamlStr, err = substituteEnvMacros(yamlStr)
	if err != nil {
		return Config{}, err
	}

	raw, macroConfig, err := resolveConfigMacros(yamlStr)
	if err != nil {
		return Config{}, err
	}

	var node yaml.Node
	if err = node.Encode(raw); err != nil {
		return Config{}, err
	}

	// Decode the resolved values into the full Config with defaults.
	config := Config{
		HealthCheckTimeout: 120,
		StartPort:          5800,
		LogLevel:           "info",
		LogTimeFormat:      "",
		LogToStdout:        LogToStdoutProxy,
		MetricsMaxInMemory: 1000,
		CaptureBuffer:      5,
		GlobalTTL:          0,
		UnloadTimeout:      DEFAULT_UNLOAD_TIMEOUT,
		UI: UIConfig{Activity: UIActivityConfig{SessionID: []string{
			"X-Session-ID",
			"X-Litellm-Session-Id",
		}}},
	}
	if err = node.Decode(&config); err != nil {
		return Config{}, err
	}
	config.Macros = macroConfig.Macros
	for modelID, modelConfig := range config.Models {
		modelConfig.Macros = macroConfig.Models[modelID].Macros
		config.Models[modelID] = modelConfig
	}

	if config.HealthCheckTimeout < 15 {
		config.HealthCheckTimeout = 15
	}

	// Apply defaults for performance config when section is missing
	if config.Performance.Every == 0 {
		config.Performance.Every = 5 * time.Second
	}
	if err = config.Performance.Validate(); err != nil {
		return Config{}, fmt.Errorf("performance: %w", err)
	}

	if config.StartPort < 1 {
		return Config{}, fmt.Errorf("startPort must be greater than 1")
	}

	if config.GlobalTTL < 0 {
		return Config{}, fmt.Errorf("globalTTL must be >= 0")
	}

	if config.UnloadTimeout < 0 {
		return Config{}, fmt.Errorf("unloadTimeout must be >= 0")
	}
	if config.UnloadTimeout == 0 {
		config.UnloadTimeout = DEFAULT_UNLOAD_TIMEOUT
	}

	config.UI.Activity.SessionID = normalizeHeaderNames(config.UI.Activity.SessionID)

	if config.Store != nil {
		if err := validateStorePath(config.Store.Path); err != nil {
			return Config{}, err
		}
	}

	if err := validateProviders(&config); err != nil {
		return Config{}, err
	}

	// Apply default for upstream.ignorePaths when not specified. The default
	// matches common static-asset suffixes so they do not trigger a swap.
	if len(config.Upstream.IgnorePaths) == 0 {
		config.Upstream.IgnorePaths = DefaultUpstreamIgnorePaths()
	}

	switch config.LogToStdout {
	case LogToStdoutProxy, LogToStdoutUpstream, LogToStdoutBoth, LogToStdoutNone:
	default:
		return Config{}, fmt.Errorf("logToStdout must be one of: proxy, upstream, both, none")
	}

	// Populate the aliases map
	config.aliases = make(map[string]string)
	for modelName, modelConfig := range config.Models {
		for _, alias := range modelConfig.Aliases {
			if _, found := config.aliases[alias]; found {
				return Config{}, fmt.Errorf("duplicate alias %s found in model: %s", alias, modelName)
			}
			config.aliases[alias] = modelName
		}
	}

	// Sort model IDs for deterministic validation and normalization.
	modelIds := make([]string, 0, len(config.Models))
	for modelId := range config.Models {
		modelIds = append(modelIds, modelId)
	}
	sort.Strings(modelIds)

	for _, modelId := range modelIds {
		modelConfig := config.Models[modelId]
		modelConfig.HealthCheckTimeout = config.HealthCheckTimeout

		if err := normalizeProviderModel(&config, modelId, &modelConfig); err != nil {
			return Config{}, err
		}

		// set model TTL to globalTTL it is the default value
		if modelConfig.UnloadAfter == MODEL_CONFIG_DEFAULT_TTL {
			modelConfig.UnloadAfter = config.GlobalTTL
		}

		if modelConfig.UnloadAfter < 0 {
			return Config{}, fmt.Errorf("model %s: invalid TTL value %d", modelId, modelConfig.UnloadAfter)
		}

		// set model unloadTimeout to the global value when left at the default
		if modelConfig.UnloadTimeout < 0 {
			return Config{}, fmt.Errorf("model %s: invalid unloadTimeout value %d", modelId, modelConfig.UnloadTimeout)
		}
		if modelConfig.UnloadTimeout == 0 {
			modelConfig.UnloadTimeout = config.UnloadTimeout
		}

		if err := modelConfig.Capabilities.Validate(); err != nil {
			return Config{}, fmt.Errorf("model %s: %w", modelId, err)
		}

		// Auto-register setParamsByID keys as aliases (skip the model's own ID)
		for key := range modelConfig.Filters.SetParamsByID {
			if key == modelId {
				continue
			}
			if _, exists := config.Models[key]; exists {
				return Config{}, fmt.Errorf("model %s filters.setParamsByID: key '%s' conflicts with an existing model ID", modelId, key)
			}
			if existingModel, exists := config.aliases[key]; exists {
				if existingModel != modelId {
					return Config{}, fmt.Errorf("duplicate alias '%s' in model %s filters.setParamsByID, already used by model %s", key, modelId, existingModel)
				}
				continue // already registered as explicit alias for this model
			}
			config.aliases[key] = modelId
			modelConfig.Aliases = append(modelConfig.Aliases, key)
		}

		if _, err := url.Parse(modelConfig.Proxy); err != nil {
			return Config{}, fmt.Errorf("model %s: invalid proxy URL: %w", modelId, err)
		}

		if modelConfig.SendLoadingState == nil {
			v := config.SendLoadingState
			modelConfig.SendLoadingState = &v
		}

		config.Models[modelId] = modelConfig
	}
	if err := validateLifecycleResidency(config); err != nil {
		return Config{}, err
	}

	// Normalize routing config. The legacy top-level `matrix`/`groups` keys and
	// the new `routing.router` block are mutually exclusive: a config may use
	// either style, never both.
	hasTopLevel := config.Matrix != nil || len(config.Groups) > 0
	rtr := config.Routing.Router
	hasRouting := rtr.Use != "" || rtr.Settings.Matrix != nil || len(rtr.Settings.Groups) > 0

	if hasTopLevel && hasRouting {
		return Config{}, fmt.Errorf("config uses both the legacy top-level 'matrix'/'groups' keys and the new 'routing.router' block; please migrate the top-level keys into 'routing.router' and remove them")
	}

	if !hasTopLevel {
		// Both groups and matrix may be defined under routing.router.settings;
		// routing.router.use selects which one is active, so there is no conflict.
		rs := config.Routing.Router.Settings
		switch config.Routing.Router.Use {
		case "matrix":
			if rs.Matrix == nil {
				return Config{}, fmt.Errorf("routing.router.use is 'matrix' but routing.router.settings.matrix is not set")
			}
			config.Matrix = rs.Matrix
		case "group", "":
			config.Groups = rs.Groups
		default:
			return Config{}, fmt.Errorf("routing.router.use: unknown router %q (valid: group, matrix)", config.Routing.Router.Use)
		}
	}

	// groups XOR matrix
	if config.Matrix != nil && len(config.Groups) > 0 {
		return Config{}, fmt.Errorf("config cannot use both 'groups' and 'matrix'")
	}

	if config.Matrix != nil {
		if err := ValidateMatrix(config.Matrix, config.Models); err != nil {
			return Config{}, fmt.Errorf("matrix: %w", err)
		}
	} else {
		config = AddDefaultGroupToConfig(config)

		// Validate group members
		memberUsage := make(map[string]string)
		for groupID, groupConfig := range config.Groups {
			prevSet := make(map[string]bool)
			for _, member := range groupConfig.Members {
				if _, found := prevSet[member]; found {
					return Config{}, fmt.Errorf("duplicate model member %s found in group: %s", member, groupID)
				}
				prevSet[member] = true

				if existingGroup, exists := memberUsage[member]; exists {
					return Config{}, fmt.Errorf("model member %s is used in multiple groups: %s and %s", member, existingGroup, groupID)
				}
				memberUsage[member] = groupID
			}
		}
	}

	// Build the canonical Config.Routing from the effective result. Both legacy
	// and new-style configs converge here. The Matrix pointer is shared so the
	// compiled matrix program stays in one place.
	if config.Matrix != nil {
		config.Routing.Router.Use = "matrix"
	} else {
		config.Routing.Router.Use = "group"
	}
	config.Routing.Router.Settings.Matrix = config.Matrix
	config.Routing.Router.Settings.Groups = config.Groups

	if config.Routing.Scheduler.Use == "" {
		config.Routing.Scheduler.Use = "fifo"
	}
	if config.Routing.Scheduler.Use != "fifo" {
		return Config{}, fmt.Errorf("routing.scheduler.use: unknown scheduler %q (valid: fifo)", config.Routing.Scheduler.Use)
	}
	for modelID := range config.Routing.Scheduler.Settings.Fifo.Priority {
		if _, found := config.RealModelName(modelID); !found {
			return Config{}, fmt.Errorf("routing.scheduler.settings.fifo.priority references unknown model %q", modelID)
		}
	}

	// Clean up hooks preload
	if len(config.Hooks.OnStartup.Preload) > 0 {
		var toPreload []string
		for _, modelID := range config.Hooks.OnStartup.Preload {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			if real, found := config.RealModelName(modelID); found {
				toPreload = append(toPreload, real)
			}
		}
		config.Hooks.OnStartup.Preload = toPreload
	}
	config.Hooks.OnStartup.Preload = appendPreferredPreloads(config, config.Hooks.OnStartup.Preload)

	// Validate API keys (env macros already substituted at string level)
	for i, apikey := range config.RequiredAPIKeys {
		if apikey == "" {
			return Config{}, fmt.Errorf("empty api key found in apiKeys")
		}
		if strings.Contains(apikey, " ") {
			return Config{}, fmt.Errorf("apiKeys[%d]: api key cannot contain spaces", i)
		}
		config.RequiredAPIKeys[i] = apikey
	}

	if err := ValidatePeerNamespace(config); err != nil {
		return Config{}, err
	}

	if err := validateSelectors(config); err != nil {
		return Config{}, err
	}

	if err := validateProfiles(config); err != nil {
		return Config{}, err
	}

	return config, nil
}

func validateProviders(config *Config) error {
	providerPools := make(map[string]string)
	for id, provider := range config.Providers {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("providers: provider names cannot be empty")
		}
		if provider.Type != ProviderTypeLemonade {
			return fmt.Errorf("providers.%s.type: unsupported provider type %q", id, provider.Type)
		}
		u, err := url.Parse(provider.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("providers.%s.baseURL: must be an absolute HTTP(S) URL", id)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("providers.%s.baseURL: unsupported URL scheme %q", id, u.Scheme)
		}
		if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("providers.%s.baseURL: credentials, query strings, and fragments are not allowed", id)
		}
		if provider.ManagementTimeout <= 0 || provider.ColdStartTimeout <= 0 || provider.DiscoveryInterval <= 0 {
			return fmt.Errorf("providers.%s: managementTimeout, coldStartTimeout, and discoveryInterval must be positive", id)
		}
		if provider.APIKeyEnv != "" {
			provider.ResolvedAPIKey = os.Getenv(provider.APIKeyEnv)
			if provider.ResolvedAPIKey == "" {
				return fmt.Errorf("providers.%s.apiKeyEnv: environment variable %q is not set", id, provider.APIKeyEnv)
			}
		}
		if provider.AdminAPIKeyEnv != "" {
			provider.ResolvedAdminAPIKey = os.Getenv(provider.AdminAPIKeyEnv)
			if provider.ResolvedAdminAPIKey == "" {
				return fmt.Errorf("providers.%s.adminApiKeyEnv: environment variable %q is not set", id, provider.AdminAPIKeyEnv)
			}
		}
		provider.BaseURL = strings.TrimRight(provider.BaseURL, "/")
		config.Providers[id] = provider
	}

	for id, pool := range config.LifecyclePools {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("lifecyclePools: pool names cannot be empty")
		}
		if _, ok := config.Providers[pool.Provider]; !ok {
			return fmt.Errorf("lifecyclePools.%s.provider references unknown provider %q", id, pool.Provider)
		}
		if existing, found := providerPools[pool.Provider]; found {
			return fmt.Errorf(
				"lifecyclePools.%s and lifecyclePools.%s use provider %q; one lifecycle pool per provider is currently supported",
				existing, id, pool.Provider,
			)
		}
		providerPools[pool.Provider] = id
		if pool.Capacity < 1 {
			return fmt.Errorf("lifecyclePools.%s.capacity must be at least 1", id)
		}
		if pool.TransientIdleTTL < 0 || pool.MaxResidentWait < 0 || pool.MaxResidentBurst < 0 {
			return fmt.Errorf("lifecyclePools.%s: transientIdleTTL, maxResidentWait, and maxResidentBurst cannot be negative", id)
		}
		if pool.ResidentFirst && pool.MaxResidentWait == 0 && pool.MaxResidentBurst == 0 {
			return fmt.Errorf("lifecyclePools.%s: residentFirst requires maxResidentWait or maxResidentBurst to prevent starvation", id)
		}
	}
	return nil
}

func validateLifecycleResidency(config Config) error {
	desired := make(map[string]int)
	for _, model := range config.Models {
		if model.Provider != "" && (model.Residency == ResidencyHardPinned || model.Residency == ResidencyPreferred) {
			desired[model.LifecyclePool]++
		}
	}
	for poolName, count := range desired {
		if capacity := config.LifecyclePools[poolName].Capacity; count > capacity {
			return fmt.Errorf(
				"lifecyclePools.%s.capacity=%d cannot hold %d hard-pinned/preferred models; increase capacity or mark lower-priority models transient",
				poolName, capacity, count,
			)
		}
	}
	return nil
}

func normalizeProviderModel(config *Config, id string, model *ModelConfig) error {
	if model.Provider == "" {
		if model.ProviderModel != "" || model.LifecyclePool != "" || model.Residency != "" {
			return fmt.Errorf("model %s: providerModel, lifecyclePool, and residency require provider", id)
		}
		return nil
	}
	provider, ok := config.Providers[model.Provider]
	if !ok {
		return fmt.Errorf("model %s: provider references unknown provider %q", id, model.Provider)
	}
	if model.ProviderModel == "" {
		return fmt.Errorf("model %s: providerModel is required for provider-backed models", id)
	}
	pool, ok := config.LifecyclePools[model.LifecyclePool]
	if !ok {
		return fmt.Errorf("model %s: lifecyclePool references unknown pool %q", id, model.LifecyclePool)
	}
	if pool.Provider != model.Provider {
		return fmt.Errorf("model %s: lifecyclePool %q belongs to provider %q, not %q", id, model.LifecyclePool, pool.Provider, model.Provider)
	}
	switch model.Residency {
	case ResidencyHardPinned, ResidencyPreferred, ResidencyTransient, ResidencyExternal:
	case "":
		model.Residency = ResidencyTransient
	default:
		return fmt.Errorf("model %s: unsupported residency %q", id, model.Residency)
	}
	if model.Cmd != "" || model.CmdStop != "" {
		return fmt.Errorf("model %s: provider-backed models cannot define cmd or cmdStop", id)
	}
	model.Proxy = provider.BaseURL
	model.CheckEndpoint = "/api/v1/health"
	model.UseModelName = model.ProviderModel

	for otherID, other := range config.Models {
		if otherID == id {
			continue
		}
		if other.Provider == model.Provider && other.ProviderModel == model.ProviderModel {
			return fmt.Errorf("models %s and %s map to the same provider model %q; use aliases instead", otherID, id, model.ProviderModel)
		}
	}
	return nil
}

func appendPreferredPreloads(config Config, current []string) []string {
	seen := make(map[string]bool, len(current))
	for _, id := range current {
		seen[id] = true
	}
	var preferred []string
	for id, model := range config.Models {
		if model.Provider != "" && (model.Residency == ResidencyHardPinned || model.Residency == ResidencyPreferred) {
			preferred = append(preferred, id)
		}
	}
	sort.Slice(preferred, func(i, j int) bool {
		mi, mj := config.Models[preferred[i]], config.Models[preferred[j]]
		if mi.ResidencyPriority != mj.ResidencyPriority {
			return mi.ResidencyPriority < mj.ResidencyPriority
		}
		return preferred[i] < preferred[j]
	})
	for _, id := range preferred {
		if !seen[id] {
			current = append(current, id)
			seen[id] = true
		}
	}
	return current
}

func validateProfiles(config Config) error {
	for profileName, profile := range config.Profiles {
		if strings.TrimSpace(profileName) == "" {
			return fmt.Errorf("profiles: profile names cannot be empty")
		}
		if len(profile.Pins) == 0 {
			return fmt.Errorf("profiles.%s.pins must contain at least one entry", profileName)
		}
		for pin, target := range profile.Pins {
			if strings.TrimSpace(pin) == "" {
				return fmt.Errorf("profiles.%s.pins: pin names cannot be empty", profileName)
			}
			if target == "" {
				continue
			}
			if _, found := config.ResolveBaseModel(target); !found {
				if _, found := config.Selectors[target]; found {
					continue
				}
				return fmt.Errorf("profiles.%s.pins.%s references unknown model %q", profileName, pin, target)
			}
		}
	}
	return nil
}

func normalizeHeaderNames(names []string) []string {
	normalized := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized
}
