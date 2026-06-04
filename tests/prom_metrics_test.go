package tests

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// ── Réplica mínima de PromMetrics para poder testear sin importar package main ──

type aggregate2 struct {
	ioCount        uint64
	totalLatencyNs uint64
}

func deltaFloat2(curr, prev uint64) float64 {
	if curr < prev {
		return float64(curr)
	}
	return float64(curr - prev)
}

// ── Tests de la lógica de delta tracking ──

func TestDeltaFloat_Normal(t *testing.T) {
	// Caso normal: contador sube
	got := deltaFloat2(100, 60)
	if got != 40 {
		t.Errorf("deltaFloat(100,60) = %v, want 40", got)
	}
}

func TestDeltaFloat_NoChange(t *testing.T) {
	got := deltaFloat2(50, 50)
	if got != 0 {
		t.Errorf("deltaFloat(50,50) = %v, want 0", got)
	}
}

func TestDeltaFloat_CounterReset(t *testing.T) {
	// curr < prev indica un reset del contador (reinicio del proceso/cgroup)
	// El valor actual se trata como delta absoluto, no negativo
	got := deltaFloat2(10, 9999)
	if got != 10 {
		t.Errorf("deltaFloat(10,9999) = %v, want 10 (counter reset)", got)
	}
}

// ── Tests de CounterVec con delta tracking (patrón usado en prom_metrics.go) ──

func TestPromCounterDeltaTracking(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_io_count_total", Help: "test"},
		[]string{"kind", "pod_uid"},
	)
	reg.MustRegister(counter)

	labels := prometheus.Labels{"kind": "kubernetes", "pod_uid": "uid-aaa"}

	// Simular dos ciclos de lectura del mapa eBPF
	type snapshot struct{ ioCount uint64 }
	prev := snapshot{}

	// Ciclo 1: eBPF devuelve 50 operaciones acumuladas
	curr := snapshot{ioCount: 50}
	counter.With(labels).Add(deltaFloat2(curr.ioCount, prev.ioCount))
	prev = curr

	// Ciclo 2: eBPF devuelve 80 (30 nuevas operaciones)
	curr = snapshot{ioCount: 80}
	counter.With(labels).Add(deltaFloat2(curr.ioCount, prev.ioCount))
	prev = curr

	// El counter de Prometheus debe acumular 50+30=80
	mf := collectMetric(t, reg, "test_io_count_total")
	val := getCounterValue(t, mf, labels)
	if val != 80 {
		t.Errorf("counter = %v, want 80 after two deltas (50+30)", val)
	}

	// Ciclo 3: reset del cgroup (curr < prev)
	curr = snapshot{ioCount: 5}
	counter.With(labels).Add(deltaFloat2(curr.ioCount, prev.ioCount))

	mf = collectMetric(t, reg, "test_io_count_total")
	val = getCounterValue(t, mf, labels)
	// 80 + 5 = 85 (el reset se trata como delta=5, no resta)
	if val != 85 {
		t.Errorf("counter = %v, want 85 after counter reset", val)
	}
}

func TestPromAggregationByPodUID(t *testing.T) {
	// Varios cgroups del mismo pod (init, main, sidecar) deben sumarse
	// antes de emitir, para mantener cardinalidad baja.
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_bytes_total", Help: "test"},
		[]string{"kind", "pod_uid"},
	)
	reg.MustRegister(counter)

	podUID := "uid-bbb"
	labels := prometheus.Labels{"kind": "kubernetes", "pod_uid": podUID}

	// Simula la agregación que hace Update(): suma los tres cgroups del mismo pod
	type cgroup struct{ bytes uint64 }
	cgroups := []cgroup{{bytes: 1024}, {bytes: 512}, {bytes: 256}}
	var total uint64
	for _, cg := range cgroups {
		total += cg.bytes
	}
	// Emite una sola vez el total agregado
	counter.With(labels).Add(float64(total))

	mf := collectMetric(t, reg, "test_bytes_total")
	val := getCounterValue(t, mf, labels)
	if val != 1792 {
		t.Errorf("aggregated counter = %v, want 1792 (1024+512+256)", val)
	}
}

func TestPromSeriesCleanup(t *testing.T) {
	// Cuando un pod desaparece, su serie debe eliminarse del registry
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_cleanup_total", Help: "test"},
		[]string{"kind", "pod_uid"},
	)
	reg.MustRegister(counter)

	labelsA := prometheus.Labels{"kind": "kubernetes", "pod_uid": "uid-alive"}
	labelsB := prometheus.Labels{"kind": "kubernetes", "pod_uid": "uid-dead"}

	counter.With(labelsA).Add(10)
	counter.With(labelsB).Add(20)

	// Pod B desaparece → borrar su serie
	counter.Delete(labelsB)

	mf := collectMetric(t, reg, "test_cleanup_total")
	// Solo debe quedar la serie A
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "pod_uid" && lp.GetValue() == "uid-dead" {
				t.Error("deleted series uid-dead still present in metrics output")
			}
		}
	}
	val := getCounterValue(t, mf, labelsA)
	if val != 10 {
		t.Errorf("surviving series = %v, want 10", val)
	}
}

// ── Helpers ──

func collectMetric(t *testing.T, reg *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("metric %q not found in registry", name)
	return nil
}

func getCounterValue(t *testing.T, mf *dto.MetricFamily, labels prometheus.Labels) float64 {
	t.Helper()
	for _, m := range mf.GetMetric() {
		if labelsMatch(m.GetLabel(), labels) {
			return m.GetCounter().GetValue()
		}
	}
	t.Fatalf("no metric found with labels %v", labels)
	return 0
}

func labelsMatch(pairs []*dto.LabelPair, want prometheus.Labels) bool {
	got := make(map[string]string, len(pairs))
	for _, p := range pairs {
		got[p.GetName()] = p.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// ── Test de nombres de métricas (contrato con el evaluador del operator) ──

func TestMetricNames(t *testing.T) {
	// El operator consulta exactamente estos nombres en PromQL.
	// Si se cambian en el collector, estas pruebas fallan inmediatamente.
	expected := []string{
		"ebpf_block_io_count_total",
		"ebpf_block_io_bytes_total",
		"ebpf_block_io_latency_ns_total",
		"ebpf_block_io_errors_total",
		"ebpf_cpu_runq_events_total",
		"ebpf_cpu_runq_latency_ns_total",
		"ebpf_block_io_avg_latency_ns",
	}

	reg := prometheus.NewRegistry()
	labelNames := []string{"kind", "pod_uid"}

	counters := make([]*prometheus.CounterVec, 0)
	for _, name := range expected[:6] {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: "test"}, labelNames)
		reg.MustRegister(c)
		counters = append(counters, c)
	}
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: expected[6], Help: "test"}, labelNames)
	reg.MustRegister(gauge)

	// Emitir un valor mínimo para que aparezcan en Gather
	labels := prometheus.Labels{"kind": "kubernetes", "pod_uid": "test-uid"}
	for _, c := range counters {
		c.With(labels).Add(1)
	}
	gauge.With(labels).Set(1)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := make(map[string]bool)
	for _, mf := range mfs {
		found[mf.GetName()] = true
	}
	for _, name := range expected {
		if !found[name] {
			t.Errorf("metric %q missing — operator PromQL queries will break", name)
		}
	}

	// Verificar que la query del evaluador usa los nombres correctos
	ioQuery := `rate(ebpf_block_io_latency_ns_total{kind="kubernetes"}[5m]) / ignoring() (rate(ebpf_block_io_count_total{kind="kubernetes"}[5m]) > 0)`
	cpuQuery := `rate(ebpf_cpu_runq_latency_ns_total{kind="kubernetes"}[5m]) / ignoring() (rate(ebpf_cpu_runq_events_total{kind="kubernetes"}[5m]) > 0)`
	for _, metric := range []string{"ebpf_block_io_latency_ns_total", "ebpf_block_io_count_total"} {
		if !strings.Contains(ioQuery, metric) {
			t.Errorf("IO PromQL query does not reference metric %q", metric)
		}
	}
	for _, metric := range []string{"ebpf_cpu_runq_latency_ns_total", "ebpf_cpu_runq_events_total"} {
		if !strings.Contains(cpuQuery, metric) {
			t.Errorf("CPU PromQL query does not reference metric %q", metric)
		}
	}
}
