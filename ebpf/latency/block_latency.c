#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * Clave temporal para correlacionar issue/complete: usar el puntero a la
 * struct request es estable, único y atómico. Sustituye al tuple
 * (dev, sector, nr_sector) que podía colisionar entre rqs simultáneas
 * sobre el mismo sector.
 */
struct start_val_t {
    u64 ts_ns;
    u32 bytes;
    u64 cgroup_id;
};

struct cgroup_io_stats_t {
    u64 io_count;
    u64 total_latency_ns;
    u64 total_bytes;
    u64 error_count;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 32768);
    __type(key, u64);                 // (struct request *) as u64
    __type(value, struct start_val_t);
} start_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16384);
    __type(key, u64);
    __type(value, struct cgroup_io_stats_t);
} stats_by_cgroup SEC(".maps");

SEC("tp_btf/block_rq_issue")
int BPF_PROG(block_rq_issue, struct request *rq)
{
    struct start_val_t val = {};
    u64 key = (u64)rq;

    val.ts_ns = bpf_ktime_get_ns();
    val.bytes = BPF_CORE_READ(rq, __data_len);
    val.cgroup_id = bpf_get_current_cgroup_id();

    bpf_map_update_elem(&start_map, &key, &val, BPF_ANY);
    return 0;
}

SEC("tp_btf/block_rq_complete")
int BPF_PROG(block_rq_complete, struct request *rq, int error, unsigned int nr_bytes)
{
    struct start_val_t *start;
    struct cgroup_io_stats_t *stats, zero = {};
    u64 now, delta, cgroup_id;
    u64 key = (u64)rq;

    start = bpf_map_lookup_elem(&start_map, &key);
    if (!start)
        return 0;

    now = bpf_ktime_get_ns();
    delta = now - start->ts_ns;
    cgroup_id = start->cgroup_id;

    stats = bpf_map_lookup_elem(&stats_by_cgroup, &cgroup_id);
    if (!stats) {
        bpf_map_update_elem(&stats_by_cgroup, &cgroup_id, &zero, BPF_ANY);
        stats = bpf_map_lookup_elem(&stats_by_cgroup, &cgroup_id);
        if (!stats) {
            bpf_map_delete_elem(&start_map, &key);
            return 0;
        }
    }

    __sync_fetch_and_add(&stats->io_count, 1);
    __sync_fetch_and_add(&stats->total_latency_ns, delta);
    __sync_fetch_and_add(&stats->total_bytes, start->bytes);
    if (error)
        __sync_fetch_and_add(&stats->error_count, 1);

    bpf_map_delete_elem(&start_map, &key);
    return 0;
}
