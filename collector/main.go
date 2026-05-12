package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const blockMapPath = "/sys/fs/bpf/block_latency/stats_by_cgroup"
const runqMapPath = "/sys/fs/bpf/runq_latency/runq_stats_by_cgroup"
const refreshInterval = 5 * time.Second
const listenAddr = ":9100"

type CgroupIOStats struct {
	IOCount        uint64
	TotalLatencyNs uint64
	TotalBytes     uint64
	ErrorCount     uint64
}

type CgroupRunqStats struct {
	Count          uint64
	TotalLatencyNs uint64
}

type Row struct {
	CgroupID   uint64
	CgroupPath string
	CgroupKind CgroupKind
	KubeInfo   KubePathInfo
	Stats      CgroupIOStats
	RunqStats  CgroupRunqStats
	AvgNs      uint64
}

var debugTUI atomic.Bool

func main() {
	if v := os.Getenv("COLLECTOR_DEBUG_TUI"); v == "1" || v == "true" {
		debugTUI.Store(true)
	}

	blockMap, err := ebpf.LoadPinnedMap(blockMapPath, nil)
	if err != nil {
		log.Fatalf("error abriendo el mapa %s: %v", blockMapPath, err)
	}
	defer blockMap.Close()

	runqMap, err := ebpf.LoadPinnedMap(runqMapPath, nil)
	if err != nil {
		log.Printf("advertencia: no se pudo abrir el mapa %s: %v", runqMapPath, err)
		runqMap = nil
	} else {
		defer runqMap.Close()
	}

	reg := prometheus.NewRegistry()
	promMetrics := NewPromMetrics(reg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go startCollectorLoop(ctx, blockMap, runqMap, promMetrics)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Collector activo. Usa /metrics\n"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	srv := &http.Server{Addr: listenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("servidor HTTP escuchando en %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func startCollectorLoop(ctx context.Context, blockMap *ebpf.Map, runqMap *ebpf.Map, promMetrics *PromMetrics) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	var resolver *CgroupResolver
	for {
		// Rebuild resolver lazily: only when we encounter unknown cgroups.
		if resolver == nil {
			r, err := BuildCgroupResolver(cgroupRoot)
			if err != nil {
				log.Printf("error construyendo resolvedor de cgroups: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				continue
			}
			resolver = r
		}

		rows, hadUnknown, err := readStats(blockMap, runqMap, resolver)
		if err != nil {
			log.Printf("error leyendo estadísticas: %v", err)
		} else {
			promMetrics.Update(rows)
			if debugTUI.Load() {
				printStats(rows)
			}
		}

		// Trigger a resolver rebuild next tick if we saw any unknown id.
		if hadUnknown {
			resolver = nil
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func readStats(blockMap *ebpf.Map, runqMap *ebpf.Map, resolver *CgroupResolver) ([]Row, bool, error) {
	rowMap := make(map[uint64]*Row)
	var hadUnknown bool

	// Iterate block map; collect stale keys to delete *after* iteration.
	var staleBlock []uint64
	{
		var key uint64
		var val CgroupIOStats
		iter := blockMap.Iterate()
		for iter.Next(&key, &val) {
			path := resolver.Resolve(key)
			if path == "<desconocido>" {
				staleBlock = append(staleBlock, key)
				hadUnknown = true
				continue
			}
			avg := uint64(0)
			if val.IOCount > 0 {
				avg = val.TotalLatencyNs / val.IOCount
			}
			rowMap[key] = &Row{
				CgroupID:   key,
				CgroupPath: path,
				CgroupKind: ClassifyCgroupPath(path),
				KubeInfo:   ParseKubePath(path),
				Stats:      val,
				AvgNs:      avg,
			}
		}
		if err := iter.Err(); err != nil {
			return nil, hadUnknown, fmt.Errorf("block iter: %w", err)
		}
	}
	for _, k := range staleBlock {
		_ = blockMap.Delete(&k)
	}

	if runqMap != nil {
		var staleRunq []uint64
		var key uint64
		var val CgroupRunqStats
		iter := runqMap.Iterate()
		for iter.Next(&key, &val) {
			if row, exists := rowMap[key]; exists {
				row.RunqStats = val
				continue
			}
			path := resolver.Resolve(key)
			if path == "<desconocido>" {
				staleRunq = append(staleRunq, key)
				hadUnknown = true
				continue
			}
			rowMap[key] = &Row{
				CgroupID:   key,
				CgroupPath: path,
				CgroupKind: ClassifyCgroupPath(path),
				KubeInfo:   ParseKubePath(path),
				RunqStats:  val,
			}
		}
		if err := iter.Err(); err != nil {
			return nil, hadUnknown, fmt.Errorf("runq iter: %w", err)
		}
		for _, k := range staleRunq {
			_ = runqMap.Delete(&k)
		}
	}

	rows := make([]Row, 0, len(rowMap))
	for _, r := range rowMap {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CgroupID < rows[j].CgroupID })
	return rows, hadUnknown, nil
}

func printStats(rows []Row) {
	fmt.Print("\033[2J\033[H")

	fmt.Println("=== BLOCK I/O STATS BY CGROUP ===")
	fmt.Printf("Actualizado: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))

	if len(rows) == 0 {
		fmt.Println("No hay entradas en stats_by_cgroup.")
		return
	}

	fmt.Printf("%-15s %-12s %-12s %-18s %-12s %-15s %-12s %-15s %-15s %15s %-15s %-s\n",
		"CGROUP_ID",
		"IO_COUNT",
		"TOTAL_MiB",
		"TOTAL_LAT_MS",
		"ERRORS",
		"AVG_LAT_MS",
		"RUNQ_EVTS",
		"RUNQ_LAT_MS",
		"KIND",
		"RUNTIME",
		"POD_UID",
		"CGROUP_PATH",
	)

	for _, row := range rows {
		fmt.Printf("%-15d %-12d %-12.2f %-18.3f %-12d %-15.3f %-12d %-15.3f %-15s %-15s %-15s %-s\n",
			row.CgroupID,
			row.Stats.IOCount,
			bytesToMiB(row.Stats.TotalBytes),
			nsToMs(row.Stats.TotalLatencyNs),
			row.Stats.ErrorCount,
			nsToMs(row.AvgNs),
			row.RunqStats.Count,
			nsToMs(row.RunqStats.TotalLatencyNs),
			formatKind(row.CgroupKind),
			row.KubeInfo.Runtime,
			row.KubeInfo.PodUID,
			row.CgroupPath,
		)
	}
}

func bytesToMiB(b uint64) float64 { return float64(b) / (1024.0 * 1024.0) }
func nsToMs(ns uint64) float64    { return float64(ns) / 1_000_000.0 }

func formatKind(kind CgroupKind) string {
	switch kind {
	case CgroupKindHost:
		return "[HOST]"
	case CgroupKindSystemd:
		return "[SYSTEMD]"
	case CgroupKindContainer:
		return "[CONTAINER]"
	case CgroupKindKubernetes:
		return "[KUBERNETES]"
	default:
		return "[UNKNOWN]"
	}
}
