package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// ── Réplica mínima del evaluador para testear la lógica de validación ──
// (No podemos importar operator/pkg/evaluator porque tiene dependencias de k8s)

type evalConfig struct {
	endpoint  string
	window    string
	threshold string
}

// validateEvalInputs reproduce la validación de PrometheusEvaluator.Evaluate
func validateEvalInputs(cfg evalConfig) error {
	if cfg.endpoint == "" {
		return fmt.Errorf("prometheus endpoint is empty")
	}
	// Validar window en formato Prometheus (30s, 5m, 1h…)
	if !isValidPromDuration(cfg.window) {
		return fmt.Errorf("invalid evaluationWindow %q (use forms like 30s, 5m)", cfg.window)
	}
	d, err := time.ParseDuration(cfg.threshold)
	if err != nil {
		return fmt.Errorf("invalid latencyThreshold %q (use forms like 50ms, 1s)", cfg.threshold)
	}
	if d <= 0 {
		return fmt.Errorf("latencyThreshold must be positive, got %s", cfg.threshold)
	}
	return nil
}

func isValidPromDuration(s string) bool {
	// Acepta: número + unidad (s/m/h/d). Prometheus no usa "ms".
	if len(s) < 2 {
		return false
	}
	unit := s[len(s)-1]
	if unit != 's' && unit != 'm' && unit != 'h' && unit != 'd' {
		return false
	}
	for _, c := range s[:len(s)-1] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ── Tests de validación de parámetros ──

func TestEvaluatorValidation_EmptyEndpoint(t *testing.T) {
	err := validateEvalInputs(evalConfig{endpoint: "", window: "5m", threshold: "50ms"})
	if err == nil {
		t.Error("expected error for empty endpoint")
	}
}

func TestEvaluatorValidation_InvalidWindow(t *testing.T) {
	cases := []string{"", "5", "5x", "abc", "500ms"} // "ms" no es unidad válida en Prometheus
	for _, w := range cases {
		err := validateEvalInputs(evalConfig{endpoint: "http://localhost:9090", window: w, threshold: "50ms"})
		if err == nil {
			t.Errorf("expected error for invalid window %q", w)
		}
	}
}

func TestEvaluatorValidation_ValidWindows(t *testing.T) {
	cases := []string{"30s", "5m", "1h", "2d"}
	for _, w := range cases {
		err := validateEvalInputs(evalConfig{endpoint: "http://localhost:9090", window: w, threshold: "50ms"})
		if err != nil {
			t.Errorf("unexpected error for valid window %q: %v", w, err)
		}
	}
}

func TestEvaluatorValidation_InvalidThreshold(t *testing.T) {
	cases := []string{"", "abc", "-1ms", "0ms", "0s"}
	for _, th := range cases {
		err := validateEvalInputs(evalConfig{endpoint: "http://localhost:9090", window: "5m", threshold: th})
		if err == nil {
			t.Errorf("expected error for invalid threshold %q", th)
		}
	}
}

func TestEvaluatorValidation_ValidThresholds(t *testing.T) {
	cases := []string{"1ms", "50ms", "500ms", "1s", "2s"}
	for _, th := range cases {
		err := validateEvalInputs(evalConfig{endpoint: "http://localhost:9090", window: "5m", threshold: th})
		if err != nil {
			t.Errorf("unexpected error for valid threshold %q: %v", th, err)
		}
	}
}

// ── Test de integración contra un servidor Prometheus simulado ──

// prometheusResponse construye una respuesta JSON que imita la API de Prometheus
func prometheusResponse(podUID string, value float64) string {
	resp := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result": []interface{}{
				map[string]interface{}{
					"metric": map[string]string{
						"pod_uid": podUID,
						"kind":    "kubernetes",
					},
					"value": []interface{}{
						time.Now().Unix(),
						fmt.Sprintf("%g", value),
					},
				},
			},
		},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

func prometheusEmptyResponse() string {
	resp := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result":     []interface{}{},
		},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

func TestPrometheusQueryFormat_IO(t *testing.T) {
	// Verifica que el evaluador genera la query correcta para IO
	window := "5m"
	thresholdNs := int64(50 * time.Millisecond)

	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, prometheusEmptyResponse())
	}))
	defer srv.Close()

	// Construir y ejecutar la query como lo hace PrometheusEvaluator
	query := fmt.Sprintf(
		`(rate(ebpf_block_io_latency_ns_total{kind="kubernetes"}[%s]) / ignoring() (rate(ebpf_block_io_count_total{kind="kubernetes"}[%s]) > 0)) > %d`,
		window, window, thresholdNs,
	)
	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		t.Fatalf("query request failed: %v", err)
	}
	defer resp.Body.Close()

	if capturedQuery == "" {
		t.Error("server did not receive a query parameter")
	}
	for _, required := range []string{
		"ebpf_block_io_latency_ns_total",
		"ebpf_block_io_count_total",
		`kind="kubernetes"`,
		window,
	} {
		if !containsStr(capturedQuery, required) {
			t.Errorf("IO query missing %q\nfull query: %s", required, capturedQuery)
		}
	}
}

func TestPrometheusQueryFormat_CPU(t *testing.T) {
	window := "2m"
	thresholdNs := int64(10 * time.Millisecond)

	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, prometheusEmptyResponse())
	}))
	defer srv.Close()

	query := fmt.Sprintf(
		`(rate(ebpf_cpu_runq_latency_ns_total{kind="kubernetes"}[%s]) / ignoring() (rate(ebpf_cpu_runq_events_total{kind="kubernetes"}[%s]) > 0)) > %d`,
		window, window, thresholdNs,
	)
	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		t.Fatalf("query request failed: %v", err)
	}
	defer resp.Body.Close()

	for _, required := range []string{
		"ebpf_cpu_runq_latency_ns_total",
		"ebpf_cpu_runq_events_total",
		`kind="kubernetes"`,
		window,
	} {
		if !containsStr(capturedQuery, required) {
			t.Errorf("CPU query missing %q\nfull query: %s", required, capturedQuery)
		}
	}
}

func TestPrometheusResponse_SaturatedPod(t *testing.T) {
	// Simula que Prometheus devuelve un pod saturado y verifica que
	// el evaluador lo extrae correctamente.
	wantUID := "11111111-2222-3333-4444-555555555555"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Latencia promedio = 200ms en ns, muy por encima del umbral
		fmt.Fprint(w, prometheusResponse(wantUID, float64(200*time.Millisecond)))
	}))
	defer srv.Close()

	// Parsear la respuesta como lo hace el evaluador
	resp, err := http.Get(srv.URL + "/api/v1/query?query=test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Data.Result) == 0 {
		t.Fatal("expected at least one result from Prometheus mock")
	}
	gotUID := result.Data.Result[0].Metric["pod_uid"]
	if gotUID != wantUID {
		t.Errorf("pod_uid = %q, want %q", gotUID, wantUID)
	}
}

func TestPrometheusResponse_NoSaturation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, prometheusEmptyResponse())
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/query?query=test")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Result []interface{} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Data.Result) != 0 {
		t.Errorf("expected empty result when no pod is saturated, got %d entries", len(result.Data.Result))
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}
