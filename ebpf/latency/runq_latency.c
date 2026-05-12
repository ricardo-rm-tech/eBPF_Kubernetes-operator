#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * Composite key (pid, start_time) — protege contra reuso de PID en sistemas
 * con churn alto de procesos cortos. start_time del task_struct es estable
 * durante la vida del proceso.
 */
struct runq_key_t {
    u32 pid;
    u64 start_time;
};

// Mapa temporal: clave compuesta -> timestamp del wakeup
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16384);
    __type(key, struct runq_key_t);
    __type(value, u64);
} start_map SEC(".maps");

// Estructura de agregación por cgroup
struct cgroup_runq_stats_t {
    u64 count;
    u64 total_latency_ns;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 8192);
    __type(key, u64);
    __type(value, struct cgroup_runq_stats_t);
} runq_stats_by_cgroup SEC(".maps");

static __always_inline u64 get_task_cgroup_id(struct task_struct *task)
{
    return BPF_CORE_READ(task, cgroups, dfl_cgrp, kn, id);
}

static __always_inline void fill_key(struct runq_key_t *k, struct task_struct *p)
{
    k->pid = BPF_CORE_READ(p, pid);
    k->start_time = BPF_CORE_READ(p, start_time);
}

static __always_inline int handle_wakeup(struct task_struct *p)
{
    struct runq_key_t key = {};
    u64 ts = bpf_ktime_get_ns();
    fill_key(&key, p);
    bpf_map_update_elem(&start_map, &key, &ts, BPF_ANY);
    return 0;
}

SEC("tp_btf/sched_wakeup")
int BPF_PROG(sched_wakeup, struct task_struct *p)
{
    return handle_wakeup(p);
}

SEC("tp_btf/sched_wakeup_new")
int BPF_PROG(sched_wakeup_new, struct task_struct *p)
{
    return handle_wakeup(p);
}

SEC("tp_btf/sched_switch")
int BPF_PROG(sched_switch, bool preempt, struct task_struct *prev, struct task_struct *next)
{
    struct runq_key_t key = {};
    u64 *tsp, ts, delta;
    u64 cgroup_id;
    struct cgroup_runq_stats_t *stats, zero = {};

    /*
     * Si prev sigue runnable (TASK_RUNNING), ha sido preempted: re-encola
     * y empieza a contar tiempo en runqueue para él también.
     */
    long prev_state = BPF_CORE_READ(prev, __state);
    if (prev_state == TASK_RUNNING) {
        struct runq_key_t pkey = {};
        u64 now = bpf_ktime_get_ns();
        fill_key(&pkey, prev);
        bpf_map_update_elem(&start_map, &pkey, &now, BPF_ANY);
    }

    fill_key(&key, next);
    tsp = bpf_map_lookup_elem(&start_map, &key);
    if (!tsp)
        return 0;

    ts = bpf_ktime_get_ns();
    delta = ts - *tsp;
    bpf_map_delete_elem(&start_map, &key);

    cgroup_id = get_task_cgroup_id(next);

    stats = bpf_map_lookup_elem(&runq_stats_by_cgroup, &cgroup_id);
    if (!stats) {
        bpf_map_update_elem(&runq_stats_by_cgroup, &cgroup_id, &zero, BPF_ANY);
        stats = bpf_map_lookup_elem(&runq_stats_by_cgroup, &cgroup_id);
        if (!stats) return 0;
    }

    __sync_fetch_and_add(&stats->count, 1);
    __sync_fetch_and_add(&stats->total_latency_ns, delta);

    return 0;
}
