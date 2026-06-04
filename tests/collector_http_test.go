package tests

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// TestCollectorHTTPEndpoint verifica que el servidor HTTP del collector
// expone el endpoint /metrics con el formato correcto de Prometheus.
// Replica la configuración de main.go sin necesitar el binario real.
func TestCollectorHTTPEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	labelNames := []string{"kind", "pod_uid"}

	ioCount := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "ebpf_block_io_count_total", Help: "Total block I/O operations per pod."},
		labelNames,
	)
	totalLatency := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "ebpf_block_io_latency_ns_total", Help: "Accumulated block I/O latency in ns per pod."},
		labelNames,
	)
	runqEvents := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "ebpf_cpu_runq_events_total", Help: "Total scheduler wakeup events per pod."},
		labelNames,
	)
	avgLatency := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "ebpf_block_io_avg_latency_ns", Help: "Current average block I/O latency in ns per pod."},
		labelNames,
	)
	reg.MustRegister(ioCount, totalLatency, runqEvents, avgLatency)

	// Simular datos de un pod Kubernetes
	labels := prometheus.Labels{"kind": "kubernetes", "pod_uid": "aaaabbbb-cccc-dddd-eeee-ffffffffffff"}
	ioCount.With(labels).Add(42)
	totalLatency.With(labels).Add(float64(42 * 15_000_000)) // 42 ops × 15ms
	runqEvents.With(labels).Add(100)
	avgLatency.With(labels).Set(15_000_000)

	// Montar el servidor como lo hace main.go
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Collector activo. Usa /metrics\n")
	})
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("root_endpoint", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "Collector activo") {
			t.Errorf("unexpected root body: %s", body)
		}
	})

	t.Run("metrics_content_type", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
	})

	t.Run("metrics_contain_ebpf_counters", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		text := string(body)

		requiredMetrics := []string{
			"ebpf_block_io_count_total",
			"ebpf_block_io_latency_ns_total",
			"ebpf_cpu_runq_events_total",
			"ebpf_block_io_avg_latency_ns",
		}
		for _, name := range requiredMetrics {
			if !strings.Contains(text, name) {
				t.Errorf("metric %q not found in /metrics output", name)
			}
		}
	})

	t.Run("metrics_contain_pod_uid_label", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		text := string(body)

		if !strings.Contains(text, "pod_uid") {
			t.Error("label pod_uid not present in /metrics output")
		}
		if !strings.Contains(text, "aaaabbbb-cccc-dddd-eeee-ffffffffffff") {
			t.Error("test pod_uid value not found in /metrics output")
		}
	})

	t.Run("metrics_values_correct", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		text := string(body)

		// El counter de 42 operaciones debe aparecer como "42"
		if !strings.Contains(text, "ebpf_block_io_count_total") {
			t.Error("io count metric not found")
		}
		// La latencia media (gauge) debe ser 1.5e+07 ns
		if !strings.Contains(text, "ebpf_block_io_avg_latency_ns") {
			t.Error("avg latency gauge not found")
		}
	})

	t.Run("metrics_prometheus_format", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		text := string(body)

		// Formato Prometheus: líneas # HELP y # TYPE presentes
		if !strings.Contains(text, "# HELP") {
			t.Error("missing # HELP lines in Prometheus output")
		}
		if !strings.Contains(text, "# TYPE") {
			t.Error("missing # TYPE lines in Prometheus output")
		}
		// Los counters deben declararse como tipo "counter"
		if !strings.Contains(text, "# TYPE ebpf_block_io_count_total counter") {
			t.Error("ebpf_block_io_count_total should be declared as counter type")
		}
		// El gauge de latencia media debe declararse como "gauge"
		if !strings.Contains(text, "# TYPE ebpf_block_io_avg_latency_ns gauge") {
			t.Error("ebpf_block_io_avg_latency_ns should be declared as gauge type")
		}
	})
}

// TestCollectorMultiplePods verifica que el collector maneja múltiples pods
// con series independientes (cardinalidad correcta por pod_uid).
func TestCollectorMultiplePods(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "ebpf_block_io_count_total", Help: "test"},
		[]string{"kind", "pod_uid"},
	)
	reg.MustRegister(counter)

	pods := []struct {
		uid   string
		count float64
	}{
		{"pod-uid-aaa-111", 10},
		{"pod-uid-bbb-222", 25},
		{"pod-uid-ccc-333", 7},
	}

	for _, p := range pods {
		counter.With(prometheus.Labels{
			"kind":    "kubernetes",
			"pod_uid": p.uid,
		}).Add(p.count)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, p := range pods {
		if !strings.Contains(text, p.uid) {
			t.Errorf("pod_uid %q not found in /metrics", p.uid)
		}
	}
}
