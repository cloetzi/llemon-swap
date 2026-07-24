package config

import (
	"strings"
	"testing"
	"time"
)

func TestConfig_LemonadeProvider(t *testing.T) {
	t.Setenv("LEMONADE_TEST_KEY", "secret")
	cfg, err := LoadConfigFromReader(strings.NewReader(`
providers:
  local:
    type: lemonade
    baseURL: http://127.0.0.1:13305/
    apiKeyEnv: LEMONADE_TEST_KEY
    managementTimeout: 30s
    discoveryInterval: 2s
lifecyclePools:
  primary:
    provider: local
    capacity: 2
    transientIdleTTL: 30s
models:
  default:
    provider: local
    providerModel: Qwen3-4B-GGUF
    lifecyclePool: primary
    residency: preferred
    residencyPriority: 1
  occasional:
    provider: local
    providerModel: Qwen3-Coder-GGUF
    lifecyclePool: primary
    residency: transient
`))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	if got := cfg.Providers["local"].BaseURL; got != "http://127.0.0.1:13305" {
		t.Fatalf("BaseURL = %q", got)
	}
	if got := cfg.Providers["local"].ResolvedAPIKey; got != "secret" {
		t.Fatalf("resolved API key = %q", got)
	}
	if got := cfg.LifecyclePools["primary"].MaxResidentBurst; got != 8 {
		t.Fatalf("MaxResidentBurst = %d", got)
	}
	if got := cfg.LifecyclePools["primary"].MaxResidentWait; got != 10*time.Second {
		t.Fatalf("MaxResidentWait = %v", got)
	}
	if got := cfg.Models["default"].UseModelName; got != "Qwen3-4B-GGUF" {
		t.Fatalf("UseModelName = %q", got)
	}
	if len(cfg.Hooks.OnStartup.Preload) != 1 || cfg.Hooks.OnStartup.Preload[0] != "default" {
		t.Fatalf("preferred preloads = %v", cfg.Hooks.OnStartup.Preload)
	}
}

func TestConfig_ExistingProcessModelStillParses(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(`
models:
  legacy:
    cmd: llama-server --port ${PORT} --model model.gguf
`))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	if cfg.Models["legacy"].Provider != "" {
		t.Fatalf("legacy model unexpectedly has provider %q", cfg.Models["legacy"].Provider)
	}
}

func TestConfig_LemonadeProviderValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown provider",
			yaml: `models: {m: {provider: missing, providerModel: p, lifecyclePool: pool}}`,
			want: `unknown provider`,
		},
		{
			name: "unknown pool",
			yaml: `
providers: {p: {type: lemonade, baseURL: "http://localhost:13305"}}
models: {m: {provider: p, providerModel: p, lifecyclePool: missing}}
`,
			want: `unknown pool`,
		},
		{
			name: "duplicate mapping",
			yaml: `
providers: {p: {type: lemonade, baseURL: "http://localhost:13305"}}
lifecyclePools: {pool: {provider: p, capacity: 1}}
models:
  a: {provider: p, providerModel: same, lifecyclePool: pool}
  b: {provider: p, providerModel: same, lifecyclePool: pool}
`,
			want: `map to the same provider model`,
		},
		{
			name: "missing secret",
			yaml: `
providers: {p: {type: lemonade, baseURL: "http://localhost:13305", apiKeyEnv: LEMON_MISSING_TEST_KEY}}
models: {m: {cmd: "server"}}
`,
			want: `is not set`,
		},
		{
			name: "preferred models exceed capacity",
			yaml: `
providers: {p: {type: lemonade, baseURL: "http://localhost:13305"}}
lifecyclePools: {pool: {provider: p, capacity: 1}}
models:
  a: {provider: p, providerModel: a, lifecyclePool: pool, residency: preferred}
  b: {provider: p, providerModel: b, lifecyclePool: pool, residency: preferred}
`,
			want: `cannot hold 2 hard-pinned/preferred models`,
		},
		{
			name: "unbounded resident preference",
			yaml: `
providers: {p: {type: lemonade, baseURL: "http://localhost:13305"}}
lifecyclePools:
  pool: {provider: p, capacity: 1, residentFirst: true, maxResidentBurst: 0, maxResidentWait: 0s}
models: {a: {provider: p, providerModel: a, lifecyclePool: pool}}
`,
			want: `requires maxResidentWait or maxResidentBurst`,
		},
		{
			name: "multiple pools on one provider",
			yaml: `
providers: {p: {type: lemonade, baseURL: "http://localhost:13305"}}
lifecyclePools:
  one: {provider: p, capacity: 1}
  two: {provider: p, capacity: 1}
models: {a: {provider: p, providerModel: a, lifecyclePool: one}}
`,
			want: `one lifecycle pool per provider is currently supported`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfigFromReader(strings.NewReader(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
