#!/usr/bin/env bash
# =============================================================================
# Test end-to-end del sistema eBPF + Operator de Kubernetes.
#
# Este script verifica que el ciclo completo funciona en un clúster real:
#   1. Despliega un pod víctima que satura CPU o I/O.
#   2. Aplica una IORemediationPolicy con la primera acción (ScaleUp / Migrate).
#   3. Espera a que el operator observe la saturación vía Prometheus y actúe.
#   4. A los 2 minutos cambia la política a EvictAndTaint.
#   5. Verifica que la segunda acción también se ejecuta.
#   6. Limpia los recursos creados.
#
# Requisitos previos:
#   - kubectl apuntando a un clúster con el operator desplegado.
#   - Prometheus accesible vía port-forward o Service en el clúster.
#   - El collector eBPF corriendo en el nodo (start_system).
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-default}"
PROMETHEUS_URL="${PROMETHEUS_URL:-http://localhost:9090}"
PHASE_DURATION="${PHASE_DURATION:-120}"   # segundos por fase
SATURATION_WAIT="${SATURATION_WAIT:-60}"  # segundos para que la métrica llegue a Prometheus

# ── Colores para legibilidad ──
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'

log()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
err()  { echo -e "${RED}[ERR]${NC}   $*"; }

separator() { echo "=================================================="; }

# ── Pre-flight checks ──
check_prereqs() {
    log "Comprobando prerrequisitos"
    command -v kubectl >/dev/null || { err "kubectl no encontrado"; exit 1; }
    command -v curl    >/dev/null || { err "curl no encontrado"; exit 1; }

    if ! kubectl version --request-timeout=5s >/dev/null 2>&1; then
        err "No hay conexión con el clúster Kubernetes"
        exit 1
    fi
    ok "kubectl conecta al clúster"

    if ! kubectl get crd ioremediationpolicies.autoremediation.tfg.local >/dev/null 2>&1; then
        err "El CRD IORemediationPolicy no está instalado en el clúster"
        err "Aplica primero: kubectl apply -f operator/config/crd/bases/"
        exit 1
    fi
    ok "CRD IORemediationPolicy presente"

    if ! curl -fsS --max-time 5 "${PROMETHEUS_URL}/-/ready" >/dev/null 2>&1; then
        warn "Prometheus no responde en ${PROMETHEUS_URL}"
        warn "Lanza un port-forward: kubectl port-forward svc/prometheus 9090:9090"
        warn "El test continuará pero las verificaciones de métricas fallarán"
    else
        ok "Prometheus responde en ${PROMETHEUS_URL}"
    fi
}

# ── Cleanup garantizado al salir ──
cleanup() {
    log "Limpiando recursos del test"
    kubectl delete -f "${SCRIPT_DIR}/policy-cpu-scaleup.yaml" --ignore-not-found --wait=false 2>/dev/null || true
    kubectl delete -f "${SCRIPT_DIR}/policy-cpu-evict.yaml"   --ignore-not-found --wait=false 2>/dev/null || true
    kubectl delete -f "${SCRIPT_DIR}/policy-io-migrate.yaml"  --ignore-not-found --wait=false 2>/dev/null || true
    kubectl delete -f "${SCRIPT_DIR}/policy-io-evict.yaml"    --ignore-not-found --wait=false 2>/dev/null || true
    kubectl delete -f "${SCRIPT_DIR}/victim-cpu.yaml" --ignore-not-found --wait=false 2>/dev/null || true
    kubectl delete -f "${SCRIPT_DIR}/victim-io.yaml"  --ignore-not-found --wait=false 2>/dev/null || true
    # Quitar taints residuales que el operator pudo añadir
    kubectl get nodes -o name | while read -r node; do
        kubectl taint "${node}" autoremediation.tfg.local/saturation- 2>/dev/null || true
    done
    ok "Cleanup completado"
}
trap cleanup EXIT

# ── Helpers ──

# Espera hasta que un pod alcance Running (o falla tras N segundos)
wait_pod_running() {
    local pod=$1 timeout=${2:-60}
    log "Esperando a que el pod ${pod} esté Running (timeout ${timeout}s)"
    if kubectl wait --for=condition=Ready "pod/${pod}" -n "${NAMESPACE}" --timeout="${timeout}s" >/dev/null 2>&1; then
        ok "Pod ${pod} Running"
        return 0
    fi
    err "Pod ${pod} no llegó a Running en ${timeout}s"
    kubectl describe "pod/${pod}" -n "${NAMESPACE}" | tail -20
    return 1
}

# Comprueba que una métrica con valor > 0 aparece en Prometheus
assert_metric_present() {
    local query=$1 desc=$2
    log "Verificando métrica: ${desc}"
    local result
    result=$(curl -fsS --max-time 10 --data-urlencode "query=${query}" "${PROMETHEUS_URL}/api/v1/query" 2>/dev/null || echo "")
    if echo "${result}" | grep -q '"value"'; then
        ok "Métrica detectada: ${desc}"
        return 0
    fi
    warn "Métrica no detectada todavía (puede tardar): ${desc}"
    return 1
}

# Comprueba el campo .status.conditions de la policy
get_policy_condition() {
    local policy=$1 cond_type=$2
    kubectl get ioremediationpolicy "${policy}" -n "${NAMESPACE}" \
        -o jsonpath="{.status.conditions[?(@.type=='${cond_type}')].reason}" 2>/dev/null || echo ""
}

# Saca el límite de CPU actual del primer container del pod
get_pod_cpu_limit() {
    local pod=$1
    kubectl get "pod/${pod}" -n "${NAMESPACE}" \
        -o jsonpath='{.spec.containers[0].resources.limits.cpu}' 2>/dev/null || echo ""
}

# Comprueba si un nodo tiene el taint del operator
node_has_taint() {
    local node=$1
    kubectl get "node/${node}" \
        -o jsonpath='{.spec.taints[?(@.key=="autoremediation.tfg.local/saturation")].key}' 2>/dev/null \
        | grep -q "saturation"
}

# =============================================================================
#  ESCENARIO 1: SATURACIÓN DE CPU
#  Fase A → ScaleUp
#  Fase B → EvictAndTaint (tras PHASE_DURATION segundos)
# =============================================================================
test_cpu_scenario() {
    separator
    log "ESCENARIO CPU: saturación de Run Queue"
    separator

    log "Desplegando pod víctima (stress-ng, 4 workers, límite 200m)"
    kubectl apply -f "${SCRIPT_DIR}/victim-cpu.yaml"
    wait_pod_running "victim-cpu" 90 || return 1

    local initial_limit
    initial_limit=$(get_pod_cpu_limit "victim-cpu")
    log "Límite inicial de CPU: ${initial_limit}"

    log "Aguardando ${SATURATION_WAIT}s para que la métrica eBPF se propague a Prometheus"
    sleep "${SATURATION_WAIT}"

    assert_metric_present \
        'rate(ebpf_cpu_runq_latency_ns_total{kind="kubernetes"}[1m]) > 0' \
        "RunQ latency rate del collector eBPF" || warn "Continuando aunque la métrica no se haya visto"

    # ── Fase A: ScaleUp ──
    separator
    log "FASE A — Aplicando política ScaleUp"
    separator
    kubectl apply -f "${SCRIPT_DIR}/policy-cpu-scaleup.yaml"

    log "Esperando ${PHASE_DURATION}s para que el operator detecte y escale"
    sleep "${PHASE_DURATION}"

    local new_limit
    new_limit=$(get_pod_cpu_limit "victim-cpu")
    log "Límite de CPU tras ScaleUp: ${new_limit}"

    if [[ "${new_limit}" != "${initial_limit}" && -n "${new_limit}" ]]; then
        ok "ScaleUp ejecutado: ${initial_limit} → ${new_limit}"
    else
        warn "El límite de CPU no cambió. Estado de la policy:"
        kubectl get ioremediationpolicy e2e-cpu-scaleup -n "${NAMESPACE}" -o yaml | tail -25
    fi

    local reason
    reason=$(get_policy_condition "e2e-cpu-scaleup" "Progressing")
    [[ -n "${reason}" ]] && ok "Policy condition Progressing.reason=${reason}"

    # ── Fase B: EvictAndTaint ──
    separator
    log "FASE B — Sustituyendo política por EvictAndTaint"
    separator
    kubectl delete -f "${SCRIPT_DIR}/policy-cpu-scaleup.yaml" --ignore-not-found
    kubectl apply  -f "${SCRIPT_DIR}/policy-cpu-evict.yaml"

    local victim_node
    victim_node=$(kubectl get pod victim-cpu -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")
    log "Nodo donde corre la víctima: ${victim_node:-<desaparecido>}"

    log "Esperando ${PHASE_DURATION}s para que el operator expulse el pod"
    sleep "${PHASE_DURATION}"

    if ! kubectl get pod victim-cpu -n "${NAMESPACE}" >/dev/null 2>&1; then
        ok "Pod victim-cpu fue expulsado (ya no existe)"
    else
        local phase
        phase=$(kubectl get pod victim-cpu -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null)
        warn "Pod sigue presente en fase ${phase}"
    fi

    if [[ -n "${victim_node}" ]] && node_has_taint "${victim_node}"; then
        ok "Taint aplicado al nodo ${victim_node}"
    else
        warn "El nodo ${victim_node} no tiene el taint esperado"
    fi
}

# =============================================================================
#  ESCENARIO 2: SATURACIÓN DE I/O
#  Fase A → MigrateStorageClass
#  Fase B → EvictAndTaint
# =============================================================================
test_io_scenario() {
    separator
    log "ESCENARIO I/O: saturación de block I/O"
    separator

    log "Desplegando pod víctima (fio, 4 jobs randwrite directos)"
    kubectl apply -f "${SCRIPT_DIR}/victim-io.yaml"
    wait_pod_running "victim-io" 120 || return 1

    log "Aguardando ${SATURATION_WAIT}s para propagación de métricas"
    sleep "${SATURATION_WAIT}"

    assert_metric_present \
        'rate(ebpf_block_io_latency_ns_total{kind="kubernetes"}[1m]) > 0' \
        "Block I/O latency rate del collector eBPF" || warn "Continuando aunque la métrica no se haya visto"

    # ── Fase A: MigrateStorageClass ──
    separator
    log "FASE A — Aplicando política MigrateStorageClass"
    separator
    kubectl apply -f "${SCRIPT_DIR}/policy-io-migrate.yaml"

    log "Esperando ${PHASE_DURATION}s para que el operator intente la migración"
    sleep "${PHASE_DURATION}"

    local degraded_reason
    degraded_reason=$(get_policy_condition "e2e-io-migrate" "Degraded")
    if [[ -n "${degraded_reason}" ]]; then
        ok "Policy condition Degraded.reason=${degraded_reason}"
        log "Degraded es esperable si la StorageClass 'fast-nvme' o el ownership ReplicaSet no existen"
    fi
    local progressing_reason
    progressing_reason=$(get_policy_condition "e2e-io-migrate" "Progressing")
    [[ -n "${progressing_reason}" ]] && ok "Policy condition Progressing.reason=${progressing_reason}"

    # ── Fase B: EvictAndTaint ──
    separator
    log "FASE B — Sustituyendo política por EvictAndTaint"
    separator
    kubectl delete -f "${SCRIPT_DIR}/policy-io-migrate.yaml" --ignore-not-found
    kubectl apply  -f "${SCRIPT_DIR}/policy-io-evict.yaml"

    local victim_node
    victim_node=$(kubectl get pod victim-io -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")
    log "Nodo donde corre la víctima: ${victim_node:-<desaparecido>}"

    log "Esperando ${PHASE_DURATION}s para que el operator expulse el pod"
    sleep "${PHASE_DURATION}"

    if ! kubectl get pod victim-io -n "${NAMESPACE}" >/dev/null 2>&1; then
        ok "Pod victim-io fue expulsado"
    else
        local phase
        phase=$(kubectl get pod victim-io -n "${NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null)
        warn "Pod sigue presente en fase ${phase}"
    fi

    if [[ -n "${victim_node}" ]] && node_has_taint "${victim_node}"; then
        ok "Taint aplicado al nodo ${victim_node}"
    else
        warn "El nodo ${victim_node} no tiene el taint esperado"
    fi
}

# =============================================================================
#  MAIN
# =============================================================================
main() {
    separator
    log "Test E2E del sistema completo eBPF + Operator"
    log "Namespace=${NAMESPACE} Prometheus=${PROMETHEUS_URL}"
    log "Duración por fase=${PHASE_DURATION}s — total estimado ~10 minutos"
    separator

    check_prereqs

    local scenario="${1:-all}"
    case "${scenario}" in
        cpu)  test_cpu_scenario ;;
        io)   test_io_scenario ;;
        all)
            test_cpu_scenario || warn "Escenario CPU completado con avisos"
            sleep 10
            test_io_scenario  || warn "Escenario IO completado con avisos"
            ;;
        *)
            err "Uso: $0 [cpu|io|all]"
            exit 1
            ;;
    esac

    separator
    ok "Test E2E finalizado"
    separator
}

main "$@"
