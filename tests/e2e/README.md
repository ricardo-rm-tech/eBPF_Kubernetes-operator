# Test end-to-end del sistema

Este test verifica el ciclo completo del sistema en un clúster real:
**eBPF (collector)** → **Prometheus** → **Operator** → **acción de remediación**.

## Qué hace

Despliega pods que saturan CPU o I/O a propósito y observa que el operator
reacciona correctamente con cada una de sus acciones implementadas.

| Escenario | Pod víctima | Métrica eBPF | Fase A (0–2 min) | Fase B (2–4 min) |
|-----------|-------------|--------------|------------------|------------------|
| CPU | `stress-ng --cpu 4` con límite 200m | `ebpf_cpu_runq_latency_ns_total` | **ScaleUp** vertical in-place | **EvictAndTaint** |
| I/O | `fio` randwrite directo, 4 jobs | `ebpf_block_io_latency_ns_total` | **MigrateStorageClass** | **EvictAndTaint** |

Duración total ≈ 10 minutos (4 fases × 2 minutos + esperas de propagación).

## Requisitos previos

Antes de lanzar el test, asegúrate de tener:

1. **El collector eBPF corriendo** en el nodo:
   ```bash
   sudo ./start_system yes
   ```

2. **El operator desplegado** en el clúster:
   ```bash
   cd operator && make install && make run
   # o `make deploy IMG=<tu-imagen>` para correrlo dentro del clúster
   ```

3. **Prometheus accesible** (port-forward si está dentro del clúster):
   ```bash
   kubectl port-forward svc/prometheus 9090:9090
   ```

4. **CRD instalado** en el clúster:
   ```bash
   kubectl apply -f operator/config/crd/bases/
   ```

## Cómo ejecutar

```bash
# Los dos escenarios completos (~10 min)
./tests/e2e/run_e2e.sh

# Solo CPU
./tests/e2e/run_e2e.sh cpu

# Solo I/O
./tests/e2e/run_e2e.sh io
```

### Variables de entorno

| Variable | Default | Descripción |
|----------|---------|-------------|
| `NAMESPACE` | `default` | Namespace donde se despliegan los pods víctima |
| `PROMETHEUS_URL` | `http://localhost:9090` | Endpoint de Prometheus para verificar métricas |
| `PHASE_DURATION` | `120` | Segundos por fase de remediación |
| `SATURATION_WAIT` | `60` | Segundos para que la métrica eBPF llegue a Prometheus |

## Qué verifica

Para cada fase, el script comprueba:

1. **El pod víctima arranca y se mantiene Running** (`kubectl wait`).
2. **La métrica eBPF aparece en Prometheus** (query con `rate() > 0`).
3. **La acción del operator se ejecuta**:
   - `ScaleUp`: el `cpu.limit` del pod **aumenta** respecto al inicial.
   - `EvictAndTaint`: el pod **desaparece** y el nodo recibe el taint
     `autoremediation.tfg.local/saturation`.
   - `MigrateStorageClass`: aparece la condition `Progressing` o
     `Degraded` en la policy (la migración requiere una `StorageClass`
     llamada `fast-nvme`; si no existe, el operator falla con un error
     claro y eso también es información útil).
4. **El status de la `IORemediationPolicy`** (`.status.conditions`) refleja
   correctamente lo ocurrido.

## Limpieza

El script registra un `trap EXIT` que **siempre** elimina:
- Los pods víctima (`victim-cpu`, `victim-io`).
- Las cuatro `IORemediationPolicy` aplicadas.
- Cualquier taint `autoremediation.tfg.local/saturation` que el operator
  haya añadido a los nodos.

Si interrumpes el script con Ctrl-C el cleanup se ejecuta igualmente.

## Interpretación de resultados

- `[OK]` verde → la fase pasó las verificaciones.
- `[WARN]` amarillo → la verificación no fue concluyente (típicamente la
  métrica todavía no se propagó o el operator no llegó a actuar antes
  del timeout). Aumenta `PHASE_DURATION` si esto ocurre.
- `[ERR]` rojo → fallo de prerrequisitos o de conexión al clúster.

## Nota sobre `MigrateStorageClass`

Esta acción requiere:
- Que el pod víctima esté detrás de un `Deployment` con un PVC.
- Que exista una `StorageClass` con el nombre `fast-nvme`.

En el test el pod víctima es standalone y usa `emptyDir`, así que la
acción **fallará con un error explícito** (`pod is not owned by a
ReplicaSet`). Esto es **comportamiento esperado** y se captura como
`Degraded` en la policy. El propósito de incluirlo es demostrar que la
acción está implementada y devuelve errores informativos en lugar de
hacer algo destructivo.

Para probar `MigrateStorageClass` en un escenario real, sustituye
`victim-io.yaml` por un `Deployment` que monte un PVC con una
`StorageClass` lenta y crea previamente la `StorageClass` rápida de
destino.
