// Package provider defines lifecycle operations for long-lived inference
// providers. Process-managed models continue to use internal/process directly;
// provider-backed process adapters use this package for load/unload/state.
package provider

import (
	"context"
	"net/http"
	"time"
)

type ModelState string

const (
	StateUnloaded  ModelState = "unloaded"
	StateLoading   ModelState = "loading"
	StateReady     ModelState = "ready"
	StateBusy      ModelState = "busy"
	StateDraining  ModelState = "draining"
	StateUnloading ModelState = "unloading"
	StateFailed    ModelState = "failed"
)

type ResidentModel struct {
	Name          string         `json:"model_name"`
	Type          string         `json:"type"`
	Device        string         `json:"device"`
	Recipe        string         `json:"recipe"`
	Status        string         `json:"status"`
	Pinned        bool           `json:"pinned"`
	Loaded        *bool          `json:"loaded,omitempty"`
	LastUse       float64        `json:"last_use"`
	RecipeOptions map[string]any `json:"recipe_options,omitempty"`
}

type Health struct {
	Status          string          `json:"status"`
	Version         string          `json:"version"`
	ModelLoaded     string          `json:"model_loaded"`
	AllModelsLoaded []ResidentModel `json:"all_models_loaded"`
	PinnedModels    map[string]int  `json:"pinned_models"`
	MaxModels       map[string]int  `json:"max_models"`
}

type DiscoveredModel struct {
	ID      string
	OwnedBy string
}

type Capabilities struct {
	Discovery bool `json:"discovery"`
	Load      bool `json:"load"`
	Unload    bool `json:"unload"`
	Pin       bool `json:"pin"`
	Inference bool `json:"inference"`
}

// Lifecycle is the provider seam used by the scheduler-facing adapter.
type Lifecycle interface {
	Capabilities() Capabilities
	Health(context.Context) (Health, error)
	Discover(context.Context) ([]DiscoveredModel, error)
	Load(context.Context, string, *bool) error
	Unload(context.Context, string) error
	UnloadAll(context.Context) error
	Pin(context.Context, string, bool) error
	BaseURL() string
	InferenceAPIKey() string
	HTTPClient() *http.Client
}

type Status struct {
	Name                   string              `json:"name"`
	Type                   string              `json:"type"`
	Capabilities           Capabilities        `json:"capabilities"`
	Healthy                bool                `json:"healthy"`
	Version                string              `json:"version,omitempty"`
	LastError              string              `json:"lastError,omitempty"`
	LastReconciled         time.Time           `json:"lastReconciled,omitempty"`
	DiscoveredModels       []string            `json:"discoveredModels,omitempty"`
	ResidentModels         []ResidentModel     `json:"residentModels,omitempty"`
	DesiredModels          []string            `json:"desiredModels,omitempty"`
	Queued                 map[string]int      `json:"queued,omitempty"`
	Active                 map[string]int      `json:"active,omitempty"`
	Transitions            map[string]string   `json:"transitions,omitempty"`
	Restoring              map[string][]string `json:"restoring,omitempty"`
	ReconcileCorrections   uint64              `json:"reconcileCorrections"`
	LoadDurationSeconds    map[string]float64  `json:"loadDurationSeconds,omitempty"`
	UnloadDurationSeconds  map[string]float64  `json:"unloadDurationSeconds,omitempty"`
	RestoreDurationSeconds map[string]float64  `json:"restoreDurationSeconds,omitempty"`
	QueueWaitSeconds       map[string]float64  `json:"queueWaitSeconds,omitempty"`
	CoalescedLoads         uint64              `json:"coalescedLoads"`
	FailedTransitions      uint64              `json:"failedTransitions"`
	ResidentAdmissions     uint64              `json:"residentAdmissions"`
	FairnessPromotions     uint64              `json:"fairnessPromotions"`
}
