# Lemonade lifecycle providers

llemon-swap integrates with a persistent [Lemonade Server](https://lemonade-server.ai/docs/). It does not start a Lemonade process per model. The existing llama-swap process path remains unchanged for models with `cmd`; provider-backed models use the same request filters, scheduler, proxy routes, streaming path, aliases, UI, and metrics.

## Provider contract and compatibility

The implementation uses only Lemonade's documented public endpoints:

| Operation | Endpoint | Relevant response data |
| --- | --- | --- |
| Health and residency | `GET /api/v1/health` | `version`, `all_models_loaded`, `pinned_models`, `max_models` |
| Discovery | `GET /api/v1/models` | OpenAI-style model list |
| Load or change pin | `POST /api/v1/load` | `model_name`, optional `pinned` |
| Unload | `POST /api/v1/unload` | `model_name` |
| Inference | existing OpenAI-compatible route | request body is forwarded except for the configured model ID |

The contract was checked against the Lemonade 10.x documentation and repository, and integration tests exercise the 10.3.0 response shape. Compatibility is capability- and response-shape-based rather than a strict semantic-version gate. An incompatible health or model response produces an actionable provider error. No `/internal/*` endpoint is used.

## Relationship to current prior art

[Olla](https://github.com/thushan/olla) already provides Lemonade discovery, health monitoring, balancing, forwarding, and management-route passthrough. Its Lemonade integration delegates loading and unloading policy to Lemonade; it does not implement llemon-swap's preferred-default displacement ledger and automatic restoration cycle.

The Lemonade proposals for richer [model loading/unloading policy (#1705)](https://github.com/lemonade-sdk/lemonade/issues/1705) and a loaded-state/queue-aware [Node Router (#1782)](https://github.com/lemonade-sdk/lemonade/issues/1782) remained open when this integration was implemented. llemon-swap therefore integrates the public lifecycle contract without depending on those proposals. Lemonade router collections and `RoutingHelper` residency solve internal router-candidate residency and are not used as a general external pool scheduler.

Lemonade's [multi-model support](https://lemonade-server.ai/docs/guide/configuration/multi-model/) remains active. llemon-swap operates in **cooperative mode**:

- it explicitly loads, unloads, pins, and unpins models that policy authorizes;
- it reconciles health immediately before destructive transitions and periodically afterward;
- it counts unconfigured external residency against pool capacity;
- it never changes Lemonade runtime capacity or eviction configuration;
- it refuses a transition if the remaining capacity is hard-pinned or externally owned.

`lifecyclePools.<name>.capacity` must not exceed Lemonade's reported `max_models.llm`. Configure Lemonade's own LLM slot limit to at least the pool capacity. Native pressure or idle eviction can still change residency; the reconciliation loop reports and corrects desired-model drift where capacity permits.

One lifecycle pool per provider is currently supported. Define another provider entry (and normally another Lemonade Server instance) for an independent capacity domain.

## Residency semantics

`residency` is llemon-swap policy, not just Lemonade's native `pinned` bit:

- `hard-pinned`: loaded at startup, natively pinned, and never selected for automatic displacement.
- `preferred`: loaded and natively pinned while stable, but may enter a controlled unpin → unload → transient load → restore → repin cycle.
- `transient`: loaded for queued work and retained for `transientIdleTTL` before restoration requires its slot.
- `external`: observed but not lifecycle-owned. It can serve only while another application keeps it resident; llemon-swap refuses to load or unload it.

Unconfigured models found in Lemonade are also externally owned. They are shown in provider status and consume capacity, but are never automatic eviction victims.

Runtime states include unloaded, loading, ready, busy, draining, unloading, restoring, and failed. A lifecycle lease begins at scheduler admission and lasts until a normal response, SSE stream, cancellation, or proxy error really ends. An active lease prevents unload.

## Scheduling and restoration

Scheduling is independent per router and lifecycle provider. Management transitions are serialized per provider, while inference proxying does not hold the lifecycle lock. Different providers can load concurrently.

Requests use two FIFO classes within each pool:

1. work for an already-ready resident model;
2. work requiring a lifecycle swap.

Resident work normally runs first. FIFO order is preserved inside each class. Cold work is promoted when either `maxResidentBurst` resident admissions have passed or the oldest cold request has waited `maxResidentWait`, whichever occurs first. At least one bound must be non-zero when `residentFirst` is enabled.

Concurrent requests for one cold model join one in-flight load. Cancellation removes a queued waiter. An admitted stream remains active until the upstream handler returns, so a model cannot be evicted between SSE chunks.

When transient work displaces a preferred model, llemon-swap records the exact victim and its prior pin state. The transient remains resident while it has active work and then for `transientIdleTTL`. Restoration:

1. reconciles current provider state;
2. marks the transient draining and unloads it;
3. reloads displaced defaults by ascending `residencyPriority`, then alias;
4. restores their native pin state;
5. retries a failed restore up to three times with 100 ms, 200 ms, and 400 ms backoff;
6. exposes a failed/degraded transition if recovery cannot complete.

New demand for the transient while restoration begins postpones restoration and reuses or reloads the transient. Demand for the preferred model joins the in-progress restoration. The idle TTL prevents an immediate repeat cycle after newly arrived transient work.

## Startup, readiness, and recovery

At startup llemon-swap validates references and secrets, connects to each provider, discovers models and residency, validates reported capacity, and starts preferred loads in deterministic priority order.

- `GET /health` is liveness: the llemon-swap process is serving HTTP.
- `GET /ready` is readiness: every required provider is healthy and every hard-pinned/preferred model on a required provider is ready.
- `required: false` makes an unreachable provider degraded rather than startup-fatal and excludes its preferred models from the readiness gate.

There is no correctness dependency on a shutdown journal. After restart, desired state comes from configuration and actual state comes from Lemonade health. A periodic reconciliation loop detects out-of-band load and unload operations.

## Configuration reference

### `providers`

| Field | Default | Meaning |
| --- | --- | --- |
| `type` | `lemonade` | Provider implementation; currently only `lemonade` |
| `baseURL` | required | Absolute HTTP(S) Lemonade URL |
| `apiKeyEnv` | empty | Environment variable containing the inference credential |
| `adminApiKeyEnv` | empty | Environment variable containing the management credential; falls back to the inference credential |
| `managementTimeout` | `3m` | Health, discovery, unload, and pin-update timeout |
| `coldStartTimeout` | `10m` | Load timeout |
| `discoveryInterval` | `5s` | Reconciliation interval |
| `insecureSkipVerify` | `false` | Disable TLS certificate verification; unsafe outside controlled development |
| `required` | `true` | Whether startup/readiness requires this provider |

Credentials are read once from the named environment variables. Empty named variables are a startup error. Client credentials are removed before proxying and are never forwarded to management calls; llemon-swap supplies the configured provider credential instead.

### `lifecyclePools`

| Field | Default | Meaning |
| --- | --- | --- |
| `provider` | required | Provider owning the pool |
| `capacity` | `1` | Maximum LLM residency llemon-swap plans against |
| `restorePreferred` | `true` | Restore displaced preferred models |
| `transientIdleTTL` | `0s` | Delay before a transient gives its slot back |
| `residentFirst` | `true` | Prefer ready resident work |
| `maxResidentBurst` | `8` | Resident admissions before cold promotion |
| `maxResidentWait` | `10s` | Maximum wait before cold promotion |

Lifecycle pools deliberately do not overload the upstream `matrix` field. A matrix still describes valid concurrent process combinations. A lifecycle pool describes capacity on one long-lived provider.

### Provider-backed model fields

| Field | Meaning |
| --- | --- |
| `provider` | Provider name |
| `providerModel` | Exact Lemonade model ID |
| `lifecyclePool` | Pool name owned by that provider |
| `residency` | `hard-pinned`, `preferred`, `transient`, or `external` |
| `residencyPriority` | Lower values restore/load first |

Provider models cannot define `cmd` or `cmdStop`. `proxy`, `checkEndpoint`, and `useModelName` are derived automatically. Duplicate mappings of two aliases to one provider model are rejected; use the existing `aliases` field on one model instead.

## Status and metrics

`GET /api/providers` and the Models UI expose health, discovered and resident models, desired defaults, native pin state, active requests, transitions, displaced restoration sets, last errors, and reconciliation corrections. `/v1/models` exposes configured aliases and provider metadata.

Prometheus output includes:

- `llemon_provider_healthy`
- `llemon_provider_resident_models`
- `llemon_provider_desired_models`
- `llemon_provider_reconcile_corrections_total`
- `llemon_provider_coalesced_loads_total`
- `llemon_provider_failed_transitions_total`
- `llemon_provider_resident_first_admissions_total`
- `llemon_provider_fairness_promotions_total`
- `llemon_model_active_requests`
- `llemon_model_queued_requests`
- `llemon_model_queue_wait_seconds`
- `llemon_model_load_duration_seconds`
- `llemon_model_unload_duration_seconds`
- `llemon_model_restoration_duration_seconds`

Lifecycle logs use `provider`, `model`, `transition`, and duration fields. Prompts and credentials are never logged.

## Security

- Prefer loopback or a private network between llemon-swap and Lemonade.
- Use HTTPS with verified certificates across hosts. `insecureSkipVerify` permits interception and should be limited to controlled development.
- Put credentials in environment variables, not YAML or command-line arguments.
- Use separate inference and management credentials when the deployment or reverse proxy supports them.
- Protect llemon-swap with `apiKeys` and firewall Lemonade's management endpoints from clients.
- Client `Authorization` and `X-API-Key` headers are stripped before inference proxying; the provider credential is injected explicitly.

## Troubleshooting

**`no_evictable_capacity`**

All effective slots are hard-pinned, externally owned, or outside the configured pool capacity. Compare `GET /api/providers` with Lemonade's health response. Increase both capacities consistently, remove an external resident model, or change a llemon-swap preferred model to transient. llemon-swap will not mutate Lemonade capacity for you.

**Provider is healthy but `/ready` returns 503**

A required hard-pinned/preferred alias has not reached ready state. Inspect its transition and `lastError` in `/api/providers`. Confirm the exact `providerModel` is registered/downloaded.

**`incompatible_provider_api`**

The server did not return the documented JSON shape. Check the Lemonade version, base URL, reverse-proxy rewrites, and [current API docs](https://lemonade-server.ai/docs/api/lemonade/).

**Streaming stops during cross-model concurrency**

Lemonade issue [#1836](https://github.com/lemonade-sdk/lemonade/issues/1836) reports incomplete streams for concurrent requests to different models with a one-model limit. It remained open when this integration was implemented. llemon-swap prevents eviction of its own active streams, but cannot correct an internal provider concurrency defect; upgrade when Lemonade publishes a fix and avoid contradictory native slot settings.

**Restoration remains failed**

The bounded retries were exhausted or external residency consumed the slot. Correct the provider condition; periodic reconciliation updates observed state, and the next relevant request/lifecycle cycle can retry. The proxy does not loop indefinitely.

## Migration from llama-swap

Existing process configurations need no changes. The Go module path, route set, aliases, filters, matrix/group DSL, profiles, peers, and process implementation remain compatible with upstream.

To add Lemonade incrementally:

1. leave existing `cmd` models in place;
2. add one `providers` entry and one `lifecyclePools` entry;
3. convert only selected models to `provider`, `providerModel`, `lifecyclePool`, and `residency`;
4. remove `cmd`, `cmdStop`, `proxy`, and manual `useModelName` from those converted models;
5. verify `/api/providers` and `/ready` before sending traffic.

The binary produced by this fork is named `llemon-swap`. Existing upstream packaging and service files may still refer to `llama-swap`; update those operational paths deliberately rather than changing the configuration format.
