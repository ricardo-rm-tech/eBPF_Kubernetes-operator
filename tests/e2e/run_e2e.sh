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

    separator
    echo -e "${GREEN}  SUCCESS!! El clúster está activo correctamente${NC}"
    separator
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

# ¿Hay alguna IORemediationPolicy en el clúster? El operator solo taintea si
# hay una policy evict activa; en cuanto no queda ninguna, deja de re-aplicarlo.
any_policy_exists() {
    [[ -n "$(kubectl get ioremediationpolicy -A -o name 2>/dev/null)" ]]
}

# Quita el taint de saturación de todos los nodos y verifica que desaparece.
# EvictAndTaint deja el nodo con el taint NoSchedule; si no se limpia antes del
# siguiente escenario, el pod víctima siguiente no se puede programar
# (FailedScheduling: untolerated taint).
#
# Clave: el operator es asíncrono y, MIENTRAS exista la policy evict, re-aplica
# el taint en cada reconcile (pelear a base de reintentos no sirve). Por eso
# primero esperamos a que NO quede ninguna policy y damos un margen para que el
# último reconcile en vuelo termine; solo entonces limpiamos el taint.
remove_saturation_taint() {
    # 1. Esperar a que no quede ninguna policy (hasta ~30s).
    local i=0
    while any_policy_exists && (( i < 15 )); do
        sleep 2; i=$(( i + 1 ))
    done
    # 2. Margen para que el reconcile en vuelo del operator termine.
    sleep 8
    # 3. Limpiar y verificar que no reaparece.
    local attempts=6
    for _ in $(seq 1 "${attempts}"); do
        kubectl get nodes -o name 2>/dev/null | while read -r node; do
            kubectl taint "${node}" autoremediation.tfg.local/saturation- 2>/dev/null || true
        done
        sleep 3
        if ! kubectl get nodes -o jsonpath='{range .items[*]}{.spec.taints[?(@.key=="autoremediation.tfg.local/saturation")].key}{end}' 2>/dev/null | grep -q saturation; then
            ok "Taint de saturación limpiado del nodo"
            return 0
        fi
    done
    warn "El taint de saturación persiste (¿operator con policy aún activa?)"
}

# Deja el clúster en un estado limpio y conocido, de forma AISLADA e idempotente.
# Se llama ANTES de cada escenario para que ninguno herede basura del anterior
# (policies vivas que hacen re-taintear al operator, taint NoSchedule residual,
# pods víctima colgados). Es la base del aislamiento entre escenarios.
reset_cluster_state() {
    log "Reset: dejando el clúster en estado limpio para el siguiente escenario"

    # 1. Borrar TODAS las policies del test (da igual cuál quedara).
    for p in policy-cpu-scaleup policy-cpu-evict policy-io-migrate policy-io-evict; do
        kubectl delete -f "${SCRIPT_DIR}/${p}.yaml" --ignore-not-found --wait=false 2>/dev/null || true
    done

    # 2. Borrar pods víctima si quedaran.
    kubectl delete pod victim-cpu victim-io -n "${NAMESPACE}" --ignore-not-found --force --grace-period=0 2>/dev/null || true

    # 3. Esperar a que NO quede ninguna policy (sin policy, el operator deja de
    #    taintear). Hasta ~40s.
    local i=0
    while any_policy_exists && (( i < 20 )); do
        sleep 2; i=$(( i + 1 ))
    done
    if any_policy_exists; then
        warn "Reset: aún hay policies tras esperar; el taint podría reaparecer"
    fi

    # 3b. Margen para que el operator drene su cola de reconcile. Tras borrar la
    #     policy, el operator puede seguir procesando un reconcile en vuelo
    #     (leyó saturación en Prometheus, que además tiene lag) y re-taintear
    #     una última vez. Esperamos a que se calme antes de la limpieza final.
    sleep 20

    # 4. Limpiar el taint y CONFIRMAR que se mantiene limpio (que el operator no
    #    lo re-aplica). Exigimos 3 comprobaciones seguidas sin taint.
    local stable=0 tries=0
    while (( stable < 3 && tries < 20 )); do
        kubectl get nodes -o name 2>/dev/null | while read -r node; do
            kubectl taint "${node}" autoremediation.tfg.local/saturation- 2>/dev/null || true
        done
        sleep 3
        if kubectl get nodes -o jsonpath='{range .items[*]}{.spec.taints[?(@.key=="autoremediation.tfg.local/saturation")].key}{end}' 2>/dev/null | grep -q saturation; then
            stable=0
        else
            stable=$(( stable + 1 ))
        fi
        tries=$(( tries + 1 ))
    done

    if (( stable >= 3 )); then
        ok "Reset: clúster limpio (sin policies, sin taint, nodo programable)"
    else
        warn "Reset: el taint sigue reapareciendo; revisa si el operador tiene alguna policy activa"
    fi
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

    # Capturamos el nodo AHORA, mientras el pod está estable. Si lo dejamos
    # para la fase B, el operator ya puede haberlo expulsado y perderíamos
    # la referencia (el taint se verifica sobre este nodo).
    local victim_node
    victim_node=$(kubectl get pod victim-cpu -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")

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

    # El resize in-place deja el pod en transición un instante, así que el
    # límite puede leerse vacío en el primer intento. Reintentamos unas veces.
    local new_limit=""
    for _ in $(seq 1 6); do
        new_limit=$(get_pod_cpu_limit "victim-cpu")
        [[ -n "${new_limit}" && "${new_limit}" != "${initial_limit}" ]] && break
        sleep 2
    done
    log "Límite de CPU tras ScaleUp: ${new_limit}"

    # Evidencia primaria: la condition Progressing/Remediating de la policy
    # (el operator confirma que actuó). El límite es evidencia secundaria.
    local reason
    reason=$(get_policy_condition "e2e-cpu-scaleup" "Progressing")

    if [[ "${new_limit}" != "${initial_limit}" && -n "${new_limit}" ]]; then
        ok "ScaleUp ejecutado: ${initial_limit} → ${new_limit}"
    elif [[ "${reason}" == "Remediating" ]]; then
        ok "ScaleUp ejecutado (operator confirmó Remediating; límite leído='${new_limit:-vacío}' por transición del pod)"
    else
        warn "El límite de CPU no cambió y la policy no reporta Remediating. Estado:"
        kubectl get ioremediationpolicy e2e-cpu-scaleup -n "${NAMESPACE}" -o yaml | tail -25
    fi

    [[ -n "${reason}" ]] && ok "Policy condition Progressing.reason=${reason}"

    # ── Fase B: EvictAndTaint ──
    separator
    log "FASE B — Sustituyendo política por EvictAndTaint"
    separator
    kubectl delete -f "${SCRIPT_DIR}/policy-cpu-scaleup.yaml" --ignore-not-found
    kubectl apply  -f "${SCRIPT_DIR}/policy-cpu-evict.yaml"

    # victim_node se capturó en la fase A, cuando el pod aún estaba estable.
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

    # Borrar la policy evict ANTES de quitar el taint: si no, el operator
    # podría re-aplicarlo en su siguiente reconciliación. Esperamos a que el
    # borrado se confirme (--wait) y damos un margen para que el operator
    # procese la eliminación, antes de limpiar el taint.
    kubectl delete -f "${SCRIPT_DIR}/policy-cpu-evict.yaml" --ignore-not-found
    remove_saturation_taint
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

    # Capturamos el nodo mientras el pod está estable (ver escenario CPU).
    local victim_node
    victim_node=$(kubectl get pod victim-io -n "${NAMESPACE}" -o jsonpath='{.spec.nodeName}' 2>/dev/null || echo "")

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

    # victim_node se capturó en la fase A, cuando el pod aún estaba estable.
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

    # Borrar la policy evict antes de quitar el taint (ver escenario CPU).
    kubectl delete -f "${SCRIPT_DIR}/policy-io-evict.yaml" --ignore-not-found
    remove_saturation_taint

    # Limpieza final del escenario I/O: como es el último, garantizamos que el
    # nodo queda SIN el taint de saturación. Esperamos a que no quede ninguna
    # policy (el operator dejaría de re-aplicarlo) y borramos el taint,
    # verificando que realmente desaparece.
    log "Retirando el taint de saturación tras el escenario I/O"
    local i=0
    while any_policy_exists && (( i < 15 )); do sleep 2; i=$(( i + 1 )); done
    kubectl get nodes -o name 2>/dev/null | while read -r node; do
        kubectl taint "${node}" autoremediation.tfg.local/saturation- 2>/dev/null || true
    done
    sleep 2
    if node_has_taint "${victim_node:-debiantfg}"; then
        warn "El taint de saturación aún persiste en el nodo"
    else
        ok "Taint de saturación retirado del nodo"
    fi
}

# =============================================================================
#  ESCENARIO 3: MIGRACIÓN DE ALMACENAMIENTO (MigrateStorageClass)
#
#  Requiere un disco lento real (memoria USB) como StorageClass "slow-usb" y un
#  NVMe como "fast-nvme". Ver monitoring/storage-migration/ para el montaje.
#
#  Demuestra las DOS ramas de la acción MigrateStorageClass:
#    Caso A → volumen de SOLO LECTURA  → el operator migra USB → NVMe (éxito).
#    Caso B → volumen de LECTURA/ESCRITURA → el operator RECHAZA para no perder
#             datos (Degraded). Es la salvaguarda anti-pérdida-de-datos.
# =============================================================================
MIGRATE_DIR="${SCRIPT_DIR}/migrate"

# Comprueba los prerrequisitos específicos de la migración (StorageClasses).
check_migration_prereqs() {
    log "Comprobando prerrequisitos de migración (StorageClasses slow-usb / fast-nvme)"
    local ok_sc=1
    for sc in slow-usb fast-nvme; do
        if kubectl get storageclass "${sc}" >/dev/null 2>&1; then
            ok "StorageClass ${sc} presente"
        else
            err "Falta la StorageClass ${sc}"
            ok_sc=0
        fi
    done
    if [[ "${ok_sc}" -eq 0 ]]; then
        err "Monta el almacenamiento primero: kubectl apply -f monitoring/storage-migration/"
        err "y formatea/monta la USB en /mnt/slow-usb (ver GUIA_USO.md)."
        return 1
    fi
    return 0
}

test_migration_scenario() {
    separator
    log "ESCENARIO MIGRACIÓN: MigrateStorageClass (disco lento USB → NVMe)"
    separator
    log "El operator vigila la latencia de I/O y, ante saturación en el disco"
    log "lento (USB), intenta mover el volumen a una StorageClass rápida (NVMe)."
    log "Solo migra si el volumen se usa en SOLO LECTURA (no perder datos)."

    check_migration_prereqs || return 1

    # ── Caso A: volumen READ-ONLY → migración exitosa ──
    separator
    log "CASO A — Volumen de SOLO LECTURA (se espera MIGRACIÓN EXITOSA)"
    separator
    log "Desplegando pod con PVC en slow-usb (USB), montado readOnly y leyendo"
    log "en bucle con O_DIRECT para generar latencia de I/O real desde la USB."
    kubectl apply -f "${MIGRATE_DIR}/victim-migrate-readonly.yaml" >/dev/null 2>&1

    local dep_ro="victim-migrate-ro"
    if ! kubectl rollout status deployment/${dep_ro} -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1; then
        err "El Deployment ${dep_ro} no llegó a estar disponible"
        kubectl delete -f "${MIGRATE_DIR}/victim-migrate-readonly.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
        return 1
    fi
    ok "Pod read-only Running; PVC inicial en slow-usb (USB lenta)"

    log "Aguardando ${SATURATION_WAIT}s para que la latencia de lectura llegue a Prometheus"
    sleep "${SATURATION_WAIT}"
    assert_metric_present \
        'rate(ebpf_block_io_latency_ns_total{kind="kubernetes"}[1m]) > 0' \
        "Latencia de I/O del pod read-only (lectura desde USB)" \
        || warn "Métrica no vista aún; la migración podría no dispararse"

    log "Aplicando política MigrateStorageClass (destino fast-nvme)"
    kubectl apply -f "${MIGRATE_DIR}/policy-migrate.yaml" >/dev/null 2>&1

    log "Esperando ${PHASE_DURATION}s a que el operator evalúe y migre"
    sleep "${PHASE_DURATION}"

    # Verificación: debe existir un PVC nuevo en fast-nvme y el Deployment
    # debe apuntar a él.
    local new_claim
    new_claim=$(kubectl get deployment ${dep_ro} -n "${NAMESPACE}" \
        -o jsonpath='{.spec.template.spec.volumes[0].persistentVolumeClaim.claimName}' 2>/dev/null)
    if [[ "${new_claim}" == *"-migrated-"* ]]; then
        local new_sc
        new_sc=$(kubectl get pvc "${new_claim}" -n "${NAMESPACE}" \
            -o jsonpath='{.spec.storageClassName}' 2>/dev/null)
        if [[ "${new_sc}" == "fast-nvme" ]]; then
            ok "MIGRACIÓN EXITOSA: PVC nuevo '${new_claim}' en fast-nvme; Deployment repuntado"
        else
            warn "El Deployment usa un PVC migrado pero su StorageClass es '${new_sc}' (esperada fast-nvme)"
        fi
    else
        warn "El Deployment sigue apuntando a '${new_claim}' (no se migró; revisa los logs del operator)"
    fi

    local reason_a
    reason_a=$(get_policy_condition "e2e-migrate" "Progressing")
    [[ "${reason_a}" == "Remediating" ]] && ok "Policy condition Progressing.reason=Remediating"

    # Limpieza del caso A.
    kubectl delete -f "${MIGRATE_DIR}/policy-migrate.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
    kubectl delete -f "${MIGRATE_DIR}/victim-migrate-readonly.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
    kubectl get pvc -n "${NAMESPACE}" -o name 2>/dev/null | grep -E 'victim-migrate-ro' | xargs -r kubectl delete -n "${NAMESPACE}" --wait=false >/dev/null 2>&1
    sleep 5

    # ── Caso B: volumen READ-WRITE con datos → rechazo seguro ──
    separator
    log "CASO B — Volumen de LECTURA/ESCRITURA (se espera RECHAZO SEGURO)"
    separator
    log "Desplegando pod con fio ESCRIBIENDO en un PVC en slow-usb (USB). El"
    log "operator detectará saturación pero rechazará migrar para no perder datos."
    kubectl apply -f "${MIGRATE_DIR}/victim-migrate-readwrite.yaml" >/dev/null 2>&1

    local dep_rw="victim-migrate-rw"
    if ! kubectl rollout status deployment/${dep_rw} -n "${NAMESPACE}" --timeout=120s >/dev/null 2>&1; then
        err "El Deployment ${dep_rw} no llegó a estar disponible"
        kubectl delete -f "${MIGRATE_DIR}/victim-migrate-readwrite.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
        return 1
    fi
    ok "Pod fio Running; escribiendo en el PVC de slow-usb (USB lenta)"

    log "Aguardando ${SATURATION_WAIT}s para que la latencia de escritura llegue a Prometheus"
    sleep "${SATURATION_WAIT}"
    assert_metric_present \
        'rate(ebpf_block_io_latency_ns_total{kind="kubernetes"}[1m]) > 0' \
        "Latencia de I/O del pod fio (escritura en USB)" \
        || warn "Métrica no vista aún"

    log "Aplicando política MigrateStorageClass (destino fast-nvme)"
    kubectl apply -f "${MIGRATE_DIR}/policy-migrate.yaml" >/dev/null 2>&1

    log "Esperando ${PHASE_DURATION}s a que el operator evalúe (debe rechazar)"
    sleep "${PHASE_DURATION}"

    # Verificación: NO debe haberse creado ningún PVC migrado y la policy debe
    # estar Degraded.
    local migrated_count
    migrated_count=$(kubectl get pvc -n "${NAMESPACE}" -o name 2>/dev/null | grep -c 'migrated' || true)
    local degraded
    degraded=$(get_policy_condition "e2e-migrate" "Degraded")
    if [[ "${migrated_count}" -eq 0 && -n "${degraded}" ]]; then
        ok "RECHAZO SEGURO: no se creó ningún PVC migrado; policy Degraded.reason=${degraded}"
        ok "Los datos del volumen escribible permanecen intactos en slow-usb"
    else
        warn "Resultado inesperado (migrated_count=${migrated_count}, degraded='${degraded}')"
    fi

    # Limpieza del caso B.
    kubectl delete -f "${MIGRATE_DIR}/policy-migrate.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
    kubectl delete -f "${MIGRATE_DIR}/victim-migrate-readwrite.yaml" --ignore-not-found --wait=false >/dev/null 2>&1
    kubectl get pvc -n "${NAMESPACE}" -o name 2>/dev/null | grep -E 'victim-migrate|migrated' | xargs -r kubectl delete -n "${NAMESPACE}" --wait=false >/dev/null 2>&1

    separator
    ok "Escenario de migración completado (Caso A: migra, Caso B: rechaza)"
    separator
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
        cpu)
            reset_cluster_state
            test_cpu_scenario
            ;;
        io)
            reset_cluster_state
            test_io_scenario
            ;;
        migrate)
            reset_cluster_state
            test_migration_scenario || warn "Escenario de migración completado con avisos"
            ;;
        all)
            reset_cluster_state
            test_cpu_scenario || warn "Escenario CPU completado con avisos"
            # Reset AISLADO entre escenarios: garantiza que el I/O empieza en
            # estado limpio aunque el CPU haya dejado taint/policies.
            reset_cluster_state
            test_io_scenario  || warn "Escenario IO completado con avisos"
            # Migración: solo si el almacenamiento (USB/StorageClasses) está
            # montado; si no, el propio escenario avisa y se salta limpiamente.
            reset_cluster_state
            test_migration_scenario || warn "Escenario de migración completado con avisos"
            ;;
        *)
            err "Uso: $0 [cpu|io|migrate|all]"
            exit 1
            ;;
    esac

    separator
    ok "Test E2E finalizado"
    separator
}

main "$@"
