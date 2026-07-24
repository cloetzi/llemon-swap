package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/provider"
	"github.com/mostlygeek/llama-swap/internal/shared"
)

// modelRecord is one entry in the OpenAI-compatible /v1/models listing.
type modelRecord struct {
	ID                  string         `json:"id"`
	Object              string         `json:"object"`
	Created             int64          `json:"created"`
	OwnedBy             string         `json:"owned_by"`
	Name                string         `json:"name,omitempty"`
	Description         string         `json:"description,omitempty"`
	Architecture        map[string]any `json:"architecture,omitempty"`
	Capabilities        map[string]any `json:"capabilities,omitempty"`
	SupportedParameters []string       `json:"supported_parameters,omitempty"`
	ContextLength       int            `json:"context_length,omitempty"`
	Meta                map[string]any `json:"meta,omitempty"`
	Status              map[string]any `json:"status"`
}

// cappedMetadataKeys are top-level /v1/models fields produced by the
// capabilities renderer. If a model's metadata block defines any of these
// keys, the renderer's values win and the metadata keys are dropped.
var cappedMetadataKeys = map[string]struct{}{
	"architecture":         {},
	"capabilities":         {},
	"supported_parameters": {},
	"context_length":       {},
}

// renderCapabilities converts a model's capabilities config into additional
// /v1/models fields. Returns zero values when caps.Empty() is true.
func renderCapabilities(caps config.ModelCapConfig) (arch map[string]any, capsMap map[string]any, params []string, ctxLen int) {
	if caps.Empty() {
		return
	}

	hasIn := len(caps.In) > 0
	hasOut := len(caps.Out) > 0

	if hasIn || hasOut {
		arch = make(map[string]any)
	}
	if hasIn {
		arch["input_modalities"] = caps.In
	}
	if hasOut {
		arch["output_modalities"] = caps.Out
	}
	if hasIn && hasOut {
		arch["modality"] = strings.Join(caps.In, "+") + "->" + strings.Join(caps.Out, "+")
	}

	// Build capabilities map only if there's something to put in it.
	if hasIn || hasOut || caps.Tools || caps.Reranker {
		capsMap = make(map[string]any)
	}

	if hasIn {
		if contains(caps.In, "image") {
			capsMap["vision"] = true
		}
	}
	if hasIn && hasOut {
		if contains(caps.In, "audio") && contains(caps.Out, "text") {
			capsMap["audio_transcriptions"] = true
		}
		if contains(caps.In, "text") && contains(caps.Out, "audio") {
			capsMap["audio_speech"] = true
		}
		if contains(caps.In, "text") && contains(caps.Out, "image") {
			capsMap["image_generation"] = true
		}
		if contains(caps.In, "image") && contains(caps.Out, "image") {
			capsMap["image_to_image"] = true
		}
	}

	if caps.Tools {
		capsMap["function_calling"] = true
		params = []string{"tools", "tool_choice"}
	}

	if caps.Reranker {
		capsMap["reranker"] = true
	}

	if caps.Context > 0 {
		ctxLen = caps.Context
	}

	return
}

// contains reports whether s is present in ss.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// filterCappedMetadata returns metadata with renderer-owned keys removed.
func filterCappedMetadata(md map[string]any) map[string]any {
	if len(md) == 0 {
		return nil
	}
	filtered := make(map[string]any, len(md))
	for k, v := range md {
		if _, capped := cappedMetadataKeys[k]; !capped {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// handleListModels serves the OpenAI-compatible model listing: local models
// (with optional aliases) plus peer models.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	created := time.Now().Unix()
	data := make([]modelRecord, 0, len(s.cfg.Models)+len(s.cfg.Selectors))
	running := s.local.RunningModels()
	modelIDs := make(map[string]struct{})

	modelStatus := func(id string) string {
		if state, ok := running[id]; ok {
			switch state {
			case process.StateStarting:
				return "loading"
			case process.StateStopping:
				return "unloading"
			default:
				return "loaded"
			}
		}
		return "unloaded"
	}

	newRecord := func(
		id, name, description string,
		metadata map[string]any,
		caps config.ModelCapConfig,
		status string,
		internalMetadata map[string]any,
	) modelRecord {
		rec := modelRecord{
			ID:          id,
			Object:      "model",
			Created:     created,
			OwnedBy:     "llemon-swap",
			Name:        strings.TrimSpace(name),
			Description: strings.TrimSpace(description),
			Status:      map[string]any{"value": status},
		}
		rec.Architecture, rec.Capabilities, rec.SupportedParameters, rec.ContextLength = renderCapabilities(caps)
		if !caps.Empty() {
			metadata = filterCappedMetadata(metadata)
		}
		llamaSwapMetadata := make(map[string]any, len(metadata)+len(internalMetadata))
		for key, value := range metadata {
			llamaSwapMetadata[key] = value
		}
		for key, value := range internalMetadata {
			llamaSwapMetadata[key] = value
		}
		if len(llamaSwapMetadata) > 0 {
			rec.Meta = map[string]any{"llamaswap": llamaSwapMetadata}
		}
		return rec
	}

	for id, mc := range s.cfg.Models {
		modelIDs[id] = struct{}{}
		for _, alias := range mc.Aliases {
			modelIDs[alias] = struct{}{}
		}

		if mc.Unlisted {
			continue
		}
		status := modelStatus(id)
		internalMetadata := map[string]any{"type": "model"}
		if mc.Provider != "" {
			internalMetadata["provider"] = mc.Provider
			internalMetadata["providerModel"] = mc.ProviderModel
			internalMetadata["lifecyclePool"] = mc.LifecyclePool
			internalMetadata["residency"] = mc.Residency
		}
		if len(mc.Aliases) > 0 {
			internalMetadata["aliases"] = mc.Aliases
		}
		data = append(data, newRecord(id, mc.Name, mc.Description, mc.Metadata, mc.Capabilities, status, internalMetadata))

		if s.cfg.IncludeAliasesInList {
			for _, alias := range mc.Aliases {
				if alias := strings.TrimSpace(alias); alias != "" {
					data = append(data, newRecord(
						alias,
						mc.Name,
						mc.Description,
						mc.Metadata,
						mc.Capabilities,
						status,
						map[string]any{"type": "alias", "modelID": id},
					))
				}
			}
		}
	}

	for peerID, peer := range s.cfg.Peers {
		for _, modelID := range peer.Models {
			fqn := config.PeerModelFQN(peerID, modelID)
			modelIDs[fqn] = struct{}{}
			if resolvedPeer, resolvedModel, found := s.cfg.ResolvePeerModel(modelID); found &&
				resolvedPeer == peerID && resolvedModel == modelID {
				modelIDs[modelID] = struct{}{}
			}
			data = append(data, newRecord(
				fqn,
				peerID+": "+modelID,
				"",
				nil,
				config.ModelCapConfig{},
				"unloaded",
				map[string]any{"type": "peer", "peerID": peerID},
			))
		}
	}

	for selectorID, selector := range s.cfg.Selectors {
		modelIDs[selectorID] = struct{}{}
		if selector.Unlisted {
			continue
		}
		status := "unloaded"
		for _, target := range selector.Targets {
			modelID, local := s.cfg.RealModelName(target)
			if local {
				state := running[modelID]
				if state == process.StateReady || state == process.StateStarting {
					status = "loaded"
				}
			}
			if selector.Strategy == config.SelectorStrategyPin || status == "loaded" {
				break
			}
		}
		data = append(data, newRecord(
			selectorID,
			selector.Name,
			selector.Description,
			selector.Metadata,
			config.ModelCapConfig{},
			status,
			map[string]any{"type": "selector"},
		))
	}

	if profile, ok := s.cfg.Profiles[s.ActiveProfile()]; ok {
		for pin, target := range profile.Pins {
			if target == "" {
				continue
			}
			if _, shadowsModel := modelIDs[pin]; shadowsModel {
				continue
			}
			data = append(data, newRecord(
				pin,
				"",
				"",
				nil,
				config.ModelCapConfig{},
				"unloaded",
				map[string]any{"type": "profile"},
			))
		}
	}

	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

	// Echo the Origin so browser clients can read the listing.
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

// runningModel is one entry in the /running listing.
type runningModel struct {
	Model       string `json:"model"`
	State       string `json:"state"`
	Cmd         string `json:"cmd"`
	Proxy       string `json:"proxy"`
	TTL         int    `json:"ttl"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleUnload stops every running local process. Peer models are remote and
// unaffected.
func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	s.local.Unload(0)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleRunning lists local processes that are not stopped, joining each model
// ID against its config for the cmd/proxy/ttl/name/description metadata.
func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	states := s.local.RunningModels()
	list := make([]runningModel, 0, len(states))
	for id, state := range states {
		mc := s.cfg.Models[id]
		list = append(list, runningModel{
			Model:       id,
			State:       string(state),
			Cmd:         mc.Cmd,
			Proxy:       mc.Proxy,
			TTL:         mc.UnloadAfter,
			Name:        mc.Name,
			Description: mc.Description,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Model < list[j].Model })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"running": list})
}

// discardResponseWriter satisfies http.ResponseWriter for preload requests,
// dropping the body while capturing the status code.
type discardResponseWriter struct {
	header http.Header
	status int
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = make(http.Header)
	}
	return d.header
}

func (d *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (d *discardResponseWriter) WriteHeader(status int) { d.status = status }

// startPreload fires a background GET / at every model named in
// Hooks.OnStartup.Preload so they are warm before the first real request.
// Preload names are already resolved to real model IDs by config loading.
func (s *Server) startPreload() {
	models := s.cfg.Hooks.OnStartup.Preload
	if len(models) == 0 {
		return
	}
	go func() {
		for _, modelID := range models {
			if !s.local.Handles(modelID) {
				s.proxylog.Warnf("preload: model %s is not a local model, skipping", modelID)
				continue
			}
			s.proxylog.Infof("preloading model: %s", modelID)

			req, err := http.NewRequestWithContext(s.shutdownCtx, http.MethodGet, "/", nil)
			if err != nil {
				continue
			}
			req = req.WithContext(shared.SetContext(req.Context(), shared.ReqContextData{Model: modelID, ModelID: modelID, Metadata: make(map[string]string)}))

			dw := &discardResponseWriter{status: http.StatusOK}
			s.local.ServeHTTP(dw, req)

			success := dw.status < http.StatusBadRequest
			if !success {
				s.proxylog.Errorf("failed to preload model %s: status %d", modelID, dw.status)
			}
			event.Emit(shared.ModelPreloadedEvent{ModelName: modelID, Success: success})
		}
	}()
}

// handleMetrics serves Prometheus-format performance metrics. Returns 503 when
// performance monitoring is disabled.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var statuses []provider.Status
	if s.providers != nil {
		statuses = s.providers.Status()
	}
	if s.perf == nil && len(statuses) == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("# performance monitor not available\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if s.perf != nil {
		s.perf.MetricsHandler().ServeHTTP(w, r)
	}
	for _, status := range statuses {
		healthy := 0
		if status.Healthy {
			healthy = 1
		}
		fmt.Fprintf(w, "llemon_provider_healthy{provider=%q} %d\n", status.Name, healthy)
		fmt.Fprintf(w, "llemon_provider_resident_models{provider=%q} %d\n", status.Name, len(status.ResidentModels))
		fmt.Fprintf(w, "llemon_provider_desired_models{provider=%q} %d\n", status.Name, len(status.DesiredModels))
		fmt.Fprintf(w, "llemon_provider_reconcile_corrections_total{provider=%q} %d\n", status.Name, status.ReconcileCorrections)
		fmt.Fprintf(w, "llemon_provider_coalesced_loads_total{provider=%q} %d\n", status.Name, status.CoalescedLoads)
		fmt.Fprintf(w, "llemon_provider_failed_transitions_total{provider=%q} %d\n", status.Name, status.FailedTransitions)
		fmt.Fprintf(w, "llemon_provider_resident_first_admissions_total{provider=%q} %d\n", status.Name, status.ResidentAdmissions)
		fmt.Fprintf(w, "llemon_provider_fairness_promotions_total{provider=%q} %d\n", status.Name, status.FairnessPromotions)
		for model, active := range status.Active {
			fmt.Fprintf(w, "llemon_model_active_requests{provider=%q,model=%q} %d\n", status.Name, model, active)
		}
		for model, queued := range status.Queued {
			fmt.Fprintf(w, "llemon_model_queued_requests{provider=%q,model=%q} %d\n", status.Name, model, queued)
		}
		for model, seconds := range status.QueueWaitSeconds {
			fmt.Fprintf(w, "llemon_model_queue_wait_seconds{provider=%q,model=%q} %g\n", status.Name, model, seconds)
		}
		for model, seconds := range status.LoadDurationSeconds {
			fmt.Fprintf(w, "llemon_model_load_duration_seconds{provider=%q,model=%q} %g\n", status.Name, model, seconds)
		}
		for model, seconds := range status.UnloadDurationSeconds {
			fmt.Fprintf(w, "llemon_model_unload_duration_seconds{provider=%q,model=%q} %g\n", status.Name, model, seconds)
		}
		for model, seconds := range status.RestoreDurationSeconds {
			fmt.Fprintf(w, "llemon_model_restoration_duration_seconds{provider=%q,model=%q} %g\n", status.Name, model, seconds)
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	var statuses []provider.Status
	if s.providers != nil {
		statuses = s.providers.Status()
	}
	ready := true
	for _, status := range statuses {
		if providerCfg := s.cfg.Providers[status.Name]; providerCfg.Required && !status.Healthy {
			ready = false
			break
		}
	}
	running := s.local.RunningModels()
	if ready {
		for alias, model := range s.cfg.Models {
			if model.Provider == "" ||
				(model.Residency != config.ResidencyHardPinned && model.Residency != config.ResidencyPreferred) ||
				!s.cfg.Providers[model.Provider].Required {
				continue
			}
			if state, found := running[alias]; !found || state != process.StateReady {
				ready = false
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]any{"ready": ready, "providers": statuses})
}

func (s *Server) handleAPIProviders(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var statuses []provider.Status
	if s.providers != nil {
		statuses = s.providers.Status()
	}
	json.NewEncoder(w).Encode(map[string]any{"providers": statuses})
}

func handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui", http.StatusFound)
}

func handleUpstreamRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/models", http.StatusFound)
}

// handleUpstream proxies ANY request under /upstream/<model>/<path> directly to
// the model's process, bypassing model dispatch by body/query inspection.
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	upstreamPath := r.PathValue("upstreamPath")

	searchName, modelID, remainingPath, found := shared.FindModelInPath(s.cfg, "/"+upstreamPath)
	if !found {
		shared.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}

	// Redirect /upstream/model to /upstream/model/ so relative URLs in upstream
	// responses resolve. 301 for GET/HEAD, 308 otherwise to preserve the method.
	if remainingPath == "/" && !strings.HasSuffix(r.URL.Path, "/") {
		newPath := "/upstream/" + searchName + "/"
		if r.URL.RawQuery != "" {
			newPath += "?" + r.URL.RawQuery
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, newPath, http.StatusMovedPermanently)
		} else {
			http.Redirect(w, r, newPath, http.StatusPermanentRedirect)
		}
		return
	}

	// Strip the /upstream/<model> prefix before forwarding.
	r.URL.Path = remainingPath
	// Pin the resolved model so the router skips body/query extraction.
	*r = *r.WithContext(shared.SetContext(r.Context(), shared.ReqContextData{Model: searchName, ModelID: modelID, Metadata: make(map[string]string)}))

	// If the path matches an upstream.ignorePaths entry and the model is
	// not already loaded, refuse the request without triggering a swap. The
	// server was not able to process the response because the model was not
	// already loaded.
	for _, re := range s.cfg.Upstream.IgnorePaths {
		if !re.MatchString(remainingPath) {
			continue
		}
		if s.local.Handles(modelID) {
			state, ok := s.local.RunningModels()[modelID]
			if !ok || state != process.StateReady {
				shared.SendResponse(w, r, http.StatusConflict,
					fmt.Sprintf("model %s is not loaded; path matches upstream.ignorePaths", modelID))
				return
			}
		}
		// Either the model is already loaded (no swap would be triggered)
		// or this is a peer model (peer proxying never swaps). Fall through
		// to normal dispatch.
		break
	}

	switch {
	case s.local.Handles(modelID):
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(modelID):
		s.peer.ServeHTTP(w, r)
	default:
		shared.SendResponse(w, r, http.StatusNotFound, "no router for model "+modelID)
	}
}
