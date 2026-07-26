package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/provider"
	"github.com/mostlygeek/llama-swap/internal/store"
)

type fakeLemonadeServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	models   []string
	loaded   map[string]bool
	pinned   map[string]bool
	loads    map[string]int
	unloads  map[string]int
	requests map[string]int
	headers  map[string]http.Header
	stream   map[string]chan struct{}

	inferenceStatus int
	inferenceBody   string
}

func newFakeLemonadeServer(t *testing.T, models ...string) *fakeLemonadeServer {
	t.Helper()
	fake := &fakeLemonadeServer{
		models:   models,
		loaded:   make(map[string]bool),
		pinned:   make(map[string]bool),
		loads:    make(map[string]int),
		unloads:  make(map[string]int),
		requests: make(map[string]int),
		headers:  make(map[string]http.Header),
		stream:   make(map[string]chan struct{}),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeLemonadeServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/health":
		f.mu.Lock()
		var resident []map[string]any
		for _, model := range f.models {
			if f.loaded[model] {
				resident = append(resident, map[string]any{
					"model_name": model,
					"type":       "llm",
					"status":     "ready",
					"pinned":     f.pinned[model],
				})
			}
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"status":            "ok",
			"version":           "10.3.0",
			"all_models_loaded": resident,
			"max_models":        map[string]int{"llm": 1},
		})
	case "/api/v1/models":
		var data []map[string]string
		for _, model := range f.models {
			data = append(data, map[string]string{"id": model, "owned_by": "lemonade"})
		}
		writeJSON(w, map[string]any{"object": "list", "data": data})
	case "/api/v1/load":
		var request struct {
			Model  string `json:"model_name"`
			Pinned *bool  `json:"pinned"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		f.mu.Lock()
		f.loaded[request.Model] = true
		if request.Pinned != nil {
			f.pinned[request.Model] = *request.Pinned
		}
		f.loads[request.Model]++
		f.mu.Unlock()
		writeJSON(w, map[string]string{"status": "success"})
	case "/api/v1/unload":
		var request struct {
			Model string `json:"model_name"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		f.mu.Lock()
		delete(f.loaded, request.Model)
		delete(f.pinned, request.Model)
		f.unloads[request.Model]++
		f.mu.Unlock()
		writeJSON(w, map[string]string{"status": "success"})
	case "/v1/chat/completions":
		if r.Header.Get("Origin") != "" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":"Origin not allowed"}`)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		f.mu.Lock()
		f.requests[request.Model]++
		f.headers[request.Model] = r.Header.Clone()
		streamDone := f.stream[request.Model]
		inferenceStatus := f.inferenceStatus
		inferenceBody := f.inferenceBody
		f.mu.Unlock()
		if inferenceStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(inferenceStatus)
			fmt.Fprint(w, inferenceBody)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"model\":%q,\"part\":1}\n\n", request.Model)
			w.(http.Flusher).Flush()
			if streamDone != nil {
				select {
				case <-streamDone:
				case <-r.Context().Done():
					return
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		writeJSON(w, map[string]any{"model": request.Model, "choices": []any{}})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func newLemonadeServerUnderTest(t *testing.T, fake *fakeLemonadeServer, ttl string) *Server {
	t.Helper()
	t.Setenv("LLEMON_SWAP_TEST_LEMONADE_API_KEY", "provider-secret")
	cfg, err := config.LoadConfigFromReader(strings.NewReader(fmt.Sprintf(`
providers:
  local:
    type: lemonade
    baseURL: %s
    apiKeyEnv: LLEMON_SWAP_TEST_LEMONADE_API_KEY
    managementTimeout: 2s
    coldStartTimeout: 2s
    discoveryInterval: 1h
lifecyclePools:
  primary:
    provider: local
    capacity: 1
    restorePreferred: true
    transientIdleTTL: %s
    residentFirst: true
    maxResidentBurst: 2
    maxResidentWait: 1s
models:
  default:
    provider: local
    providerModel: provider-default
    lifecyclePool: primary
    residency: preferred
  occasional:
    provider: local
    providerModel: provider-transient
    lifecyclePool: primary
    residency: transient
`, fake.server.URL, ttl)))
	if err != nil {
		t.Fatalf("LoadConfigFromReader: %v", err)
	}
	st, err := store.New("")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	discard := logmon.NewWriter(io.Discard)
	server, err := New(cfg, discard, discard, discard, nil, st, BuildInfo{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		server.CloseStreams()
		_ = server.Shutdown(2 * time.Second)
	})
	waitFor(t, 2*time.Second, func() bool {
		manager, ok := server.providers.Manager("local")
		if !ok || manager.State("default") != provider.StateReady {
			return false
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.loaded["provider-default"] && fake.pinned["provider-default"]
	})
	return server
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestServer_LemonadeProxyRewritesOnlyModel(t *testing.T) {
	fake := newFakeLemonadeServer(t, "provider-default", "provider-transient")
	server := newLemonadeServerUnderTest(t, fake, time.Hour.String())

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	if body["model"] != "provider-default" {
		t.Fatalf("provider response model = %v", body["model"])
	}
}

func TestServer_LemonadePlaygroundOriginBoundary(t *testing.T) {
	tests := []struct {
		name       string
		stream     bool
		status     int
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "normal response",
			wantStatus: http.StatusOK,
			wantBody:   `"model":"provider-default"`,
		},
		{
			name:       "error response",
			status:     http.StatusUnprocessableEntity,
			body:       `{"error":"invalid chat request"}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantBody:   `{"error":"invalid chat request"}`,
		},
		{
			name:       "streaming response",
			stream:     true,
			wantStatus: http.StatusOK,
			wantBody:   "data: [DONE]\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeLemonadeServer(t, "provider-default")
			fake.inferenceStatus = test.status
			fake.inferenceBody = test.body
			server := newLemonadeServerUnderTest(t, fake, time.Hour.String())
			server.cfg.RequiredAPIKeys = []string{"client-secret"}
			server.routes()

			requestBody := fmt.Sprintf(`{"model":"default","messages":[{"role":"user","content":"hello"}],"stream":%t}`, test.stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://llemon-swap.test")
			request.Header.Set("Authorization", "Bearer client-secret")
			request.Header.Set("X-API-Key", "other-client-secret")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.wantBody) {
				t.Errorf("body = %q, want substring %q", response.Body.String(), test.wantBody)
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
			}

			fake.mu.Lock()
			upstreamHeaders := fake.headers["provider-default"].Clone()
			fake.mu.Unlock()
			if got := upstreamHeaders.Get("Origin"); got != "" {
				t.Errorf("provider Origin = %q, want empty", got)
			}
			if got := upstreamHeaders.Get("Authorization"); got != "Bearer provider-secret" {
				t.Errorf("provider Authorization = %q, want provider credential", got)
			}
			if got := upstreamHeaders.Get("X-API-Key"); got != "" {
				t.Errorf("provider X-API-Key = %q, want client credential stripped", got)
			}
		})
	}
}

func TestServer_LemonadeTransientRestoresPreferred(t *testing.T) {
	fake := newFakeLemonadeServer(t, "provider-default", "provider-transient")
	server := newLemonadeServerUnderTest(t, fake, "1ms")

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"occasional","messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}

	waitFor(t, 2*time.Second, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.loaded["provider-default"] && fake.pinned["provider-default"] &&
			!fake.loaded["provider-transient"] && fake.unloads["provider-default"] == 1
	})
}

func TestServer_LemonadeStreamingModelNotUnloaded(t *testing.T) {
	fake := newFakeLemonadeServer(t, "provider-default", "provider-transient")
	streamDone := make(chan struct{})
	fake.stream["provider-default"] = streamDone
	server := newLemonadeServerUnderTest(t, fake, time.Hour.String())

	streamFinished := make(chan struct{})
	go func() {
		defer close(streamFinished)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"default","messages":[],"stream":true}`))
		request.Header.Set("Content-Type", "application/json")
		server.ServeHTTP(httptest.NewRecorder(), request)
	}()
	waitFor(t, time.Second, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.requests["provider-default"] == 1
	})

	transientDone := make(chan struct{})
	go func() {
		defer close(transientDone)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"occasional","messages":[]}`))
		request.Header.Set("Content-Type", "application/json")
		server.ServeHTTP(httptest.NewRecorder(), request)
	}()
	time.Sleep(20 * time.Millisecond)
	fake.mu.Lock()
	unloadedWhileStreaming := fake.unloads["provider-default"]
	fake.mu.Unlock()
	if unloadedWhileStreaming != 0 {
		t.Fatalf("preferred model unloaded %d times during active stream", unloadedWhileStreaming)
	}

	close(streamDone)
	select {
	case <-streamFinished:
	case <-time.After(time.Second):
		t.Fatal("stream did not finish")
	}
	select {
	case <-transientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("queued transient request did not finish")
	}
}

func TestServer_LemonadeQueuedCancellationAvoidsLoad(t *testing.T) {
	fake := newFakeLemonadeServer(t, "provider-default", "provider-transient")
	streamDone := make(chan struct{})
	fake.stream["provider-default"] = streamDone
	server := newLemonadeServerUnderTest(t, fake, time.Hour.String())

	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"default","messages":[],"stream":true}`))
		request.Header.Set("Content-Type", "application/json")
		server.ServeHTTP(httptest.NewRecorder(), request)
	}()
	waitFor(t, time.Second, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.requests["provider-default"] == 1
	})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"occasional","messages":[]}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done
	close(streamDone)
	time.Sleep(20 * time.Millisecond)

	fake.mu.Lock()
	loads := fake.loads["provider-transient"]
	fake.mu.Unlock()
	if loads != 0 {
		t.Fatalf("cancelled queued model loaded %d times", loads)
	}
}
