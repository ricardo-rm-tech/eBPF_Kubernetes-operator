package main

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// PromMetrics exposes the eBPF-derived stats as Prometheus counters. We use
// CounterVec (not GaugeVec) because the underlying eBPF maps store monotonic
// totals; rate()/increase() in PromQL only make sense over counters.
//
// To avoid the Reset()+Set() scrape race we keep the *current* values per
// label set and only call Add() with the positive delta. On counter reset
// (cgroup recreated, collector restart) the prior series simply stops
// growing — Prometheus handles that via the standard counter-reset logic
// because each cgroup gets a fresh series after a process restart.
type PromMetrics struct {
	mu sync.Mutex

	ioCount        *prometheus.CounterVec
	totalBytes     *prometheus.CounterVec
	totalLatencyNs *prometheus.CounterVec
	errorCount     *prometheus.CounterVec
	runqEvents     *prometheus.CounterVec
	runqLatencyNs  *prometheus.CounterVec
	avgLatencyNs   *prometheus.GaugeVec // average is a derived gauge

	prev map[string]aggregate
}

type aggregate struct {
	ioCount        uint64
	totalBytes     uint64
	totalLatencyNs uint64
	errorCount     uint64
	runqEvents     uint64
	runqLatencyNs  uint64
	avg            uint64
}

var labelNames = []string{"kind", "pod_uid"}

func NewPromMetrics(reg prometheus.Registerer) *PromMetrics {
	pm := &PromMetrics{
		ioCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "ebpf_block_io_count_total", Help: "Total block I/O operations per pod."},
			labelNames,
		),
		totalBytes: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "ebpf_block_io_bytes_total", Help: "Total bytes processed by block I/O per pod."},
			labelNames,
		),
		totalLatencyNs: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "ebpf_block_io_latency_ns_total", Help: "Accumulated block I/O latency in ns per pod."},
			labelNames,
		),
		errorCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "ebpf_block_io_errors_total", Help: "Total block I/O errors per pod."},
			labelNames,
		),
		runqEvents: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "ebpf_cpu_runq_events_total", Help: "Total scheduler wakeup events per pod."},
			labelNames,
		),
		runqLatencyNs: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "ebpf_cpu_runq_latency_ns_total", Help: "Accumulated CPU run-queue latency in ns per pod."},
			labelNames,
		),
		avgLatencyNs: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{Name: "ebpf_block_io_avg_latency_ns", Help: "Current average block I/O latency in ns per pod."},
			labelNames,
		),
		prev: make(map[string]aggregate),
	}

	reg.MustRegister(pm.ioCount, pm.totalBytes, pm.totalLatencyNs, pm.errorCount,
		pm.runqEvents, pm.runqLatencyNs, pm.avgLatencyNs)
	return pm
}

func (pm *PromMetrics) Update(rows []Row) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Multiple cgroups (init / main / sidecars) can map to the same pod_uid.
	// Aggregate them to keep cardinality bounded.
	current := make(map[string]aggregate)
	keyLabels := make(map[string]prometheus.Labels)

	for _, r := range rows {
		labels := prometheus.Labels{
			"kind":    string(r.CgroupKind),
			"pod_uid": r.KubeInfo.PodUID,
		}
		key := labels["kind"] + "|" + labels["pod_uid"]
		a := current[key]
		a.ioCount += r.Stats.IOCount
		a.totalBytes += r.Stats.TotalBytes
		a.totalLatencyNs += r.Stats.TotalLatencyNs
		a.errorCount += r.Stats.ErrorCount
		a.runqEvents += r.RunqStats.Count
		a.runqLatencyNs += r.RunqStats.TotalLatencyNs
		if a.ioCount > 0 {
			a.avg = a.totalLatencyNs / a.ioCount
		}
		current[key] = a
		keyLabels[key] = labels
	}

	for key, a := range current {
		labels := keyLabels[key]
		p := pm.prev[key]
		pm.ioCount.With(labels).Add(deltaFloat(a.ioCount, p.ioCount))
		pm.totalBytes.With(labels).Add(deltaFloat(a.totalBytes, p.totalBytes))
		pm.totalLatencyNs.With(labels).Add(deltaFloat(a.totalLatencyNs, p.totalLatencyNs))
		pm.errorCount.With(labels).Add(deltaFloat(a.errorCount, p.errorCount))
		pm.runqEvents.With(labels).Add(deltaFloat(a.runqEvents, p.runqEvents))
		pm.runqLatencyNs.With(labels).Add(deltaFloat(a.runqLatencyNs, p.runqLatencyNs))
		pm.avgLatencyNs.With(labels).Set(float64(a.avg))
		pm.prev[key] = a
	}

	// Drop series whose pods disappeared this cycle.
	for key := range pm.prev {
		if _, still := current[key]; still {
			continue
		}
		labels := labelsFromKey(key)
		pm.ioCount.Delete(labels)
		pm.totalBytes.Delete(labels)
		pm.totalLatencyNs.Delete(labels)
		pm.errorCount.Delete(labels)
		pm.runqEvents.Delete(labels)
		pm.runqLatencyNs.Delete(labels)
		pm.avgLatencyNs.Delete(labels)
		delete(pm.prev, key)
	}
}

func deltaFloat(curr, prev uint64) float64 {
	if curr < prev {
		// counter reset (cgroup recreated). Treat current value as the delta.
		return float64(curr)
	}
	return float64(curr - prev)
}

func labelsFromKey(key string) prometheus.Labels {
	kind, podUID := "", ""
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			kind = key[:i]
			podUID = key[i+1:]
			break
		}
	}
	return prometheus.Labels{"kind": kind, "pod_uid": podUID}
}
