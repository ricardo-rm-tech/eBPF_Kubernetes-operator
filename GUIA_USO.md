# Guía de uso del sistema

Este documento explica los tres bloques operativos del proyecto:

1. **Arrancar el sistema completo** (eBPF + collector + Prometheus + operator + Grafana).
2. **Lanzar la batería de tests** (unitarios y end-to-end).
3. **Interpretar los resultados**.

---

## Requisitos previos

Antes de empezar, verifica que tienes instalado:

| Herramienta | Versión mínima | Comprobar con |
|-------------|----------------|---------------|
| Linux kernel | 6.1 (recomendado 6.12) | `uname -r` |
| Go | 1.22 | `go version` |
| Clang / LLVM | 14 | `clang --version` |
| bpftool | reciente | `bpftool version` |
| kubectl | 1.27+ | `kubectl version --client` |
| Kubernetes | 1.27+ (para InPlacePodVerticalScaling) | `kubectl version` |
| Prometheus | 2.x | descargado en `~/prometheus-lab/` |
| Docker | reciente (opcional, solo para Grafana) | `docker version` |

---

## 1. Arrancar el sistema completo

### Paso 1.1 — Compilar los componentes (solo la primera vez)

```bash
cd ~/tfg

# Compilar los programas eBPF (genera *.o en ebpf/)
cd ebpf/latency
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I.. \
    -c block_latency.c -o ../block_latency.o
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 -I.. \
    -c runq_latency.c -o ../runq_latency.o
cd ../..

# Compilar el collector en Go
cd collector && go build -o collector . && cd ..

# Compilar el operator (binario en operator/bin/manager)
cd operator && make build && cd ..
```

### Paso 1.2 — Lanzar el sistema base (eBPF + collector + Prometheus)

El script `start_system` orquesta el arranque del nodo, carga los programas eBPF, levanta Prometheus y el collector.

```bash
cd ~/tfg
sudo ./start_system yes
```

Argumentos:
- `yes` → además de eBPF y collector, **arranca Prometheus**.
- sin argumentos → solo carga eBPF y collector (asume Prometheus ya activo).

Al terminar deberías ver:
```
collector HTTP: OK
prometheus UI: http://localhost:9090
collector metrics: http://localhost:9100/metrics
```

Verifica que las métricas eBPF están saliendo:
```bash
curl -s http://localhost:9100/metrics | grep "^ebpf_" | head
```

### Paso 1.3 — Desplegar el CRD y el operator en Kubernetes

En otra terminal:

```bash
cd ~/tfg/operator

# Instalar el CRD IORemediationPolicy en el clúster
make install

# Lanzar el controller (en primer plano, contra el clúster activo de kubectl)
make run
```

El operator empieza a observar `IORemediationPolicy` y a consultar a Prometheus.

### Paso 1.4 — Aplicar una política para empezar a remediar

**Importante:** abre **otra terminal** (la del paso 1.3 sigue ocupada por `make run` mostrando los logs del operator) y vuelve a la raíz del proyecto antes de aplicar la política:

```bash
cd ~/tfg
kubectl apply -f tests/e2e/policy-cpu-scaleup.yaml
kubectl get ioremediationpolicy
```

El sistema queda en marcha: cualquier pod con la etiqueta `app=victim, saturation-type=cpu` que sature el Run Queue será escalado verticalmente por el operator.

### Paso 1.5 — Visualizar las métricas en Grafana (opcional)

Además de la UI de Prometheus (`http://localhost:9090`), el proyecto incluye un
**Grafana auto-provisionado** con un dashboard listo para usar. Vive en
`monitoring/` y se levanta con Docker Compose.

**Requisitos:** Docker en marcha y el sistema base corriendo (paso 1.2), porque
Grafana lee del Prometheus del host (`localhost:9090`).

```bash
cd ~/tfg/monitoring
docker compose up -d
# Si Docker pide permisos de grupo, envuelve el comando:
#   sg docker -c 'cd ~/tfg/monitoring && docker compose up -d'
```

Al terminar:

```
Grafana UI  → http://localhost:3000   (admin / admin, acceso anónimo activado)
Dashboard   → "eBPF — Observabilidad y Remediación (TFG)"
```

El dashboard (refresco cada 5 s) incluye cinco paneles:

| Panel | Métrica eBPF | Para qué sirve |
|-------|--------------|----------------|
| **CPU — Latencia de Run Queue por pod** | `ebpf_cpu_runq_latency_ns_total` | Métrica que dispara `ScaleUp`; línea de umbral a 50 ms |
| **I/O — Latencia de disco por pod** | `ebpf_block_io_latency_ns_total` | Métrica que dispara `MigrateStorageClass` / `EvictAndTaint`; umbral a 50 ms |
| **I/O — Throughput por pod** | `ebpf_block_io_bytes_total` | Bytes/s de disco por pod |
| **I/O — Errores por pod** | `ebpf_block_io_errors_total` | Cualquier valor > 0 indica fallos de I/O |
| **Visión global por cgroup** | latencias por `kind` | Compara host vs container vs kubernetes; demuestra la visibilidad de kernel |

Los pods identificados con `pod_uid` aparecen automáticamente en los paneles a
medida que el collector los detecta. Al lanzar el test E2E (sección C) verás las
curvas de los pods víctima subir al saturar y bajar tras la remediación.

**Estructura de la configuración** (todo versionado en `monitoring/`):

```
monitoring/
├── docker-compose.yml                              # Grafana en network_mode: host
└── grafana/
    ├── provisioning/datasources/prometheus.yml     # datasource auto-conectado
    ├── provisioning/dashboards/dashboards.yml       # provider de dashboards
    └── dashboards/ebpf-remediation.json            # dashboard del TFG
```

Para parar Grafana:

```bash
cd ~/tfg/monitoring && docker compose down
```

---

## 2. Ejecutar los tests

El proyecto tiene **dos niveles de tests** independientes:

### Nivel A — Tests unitarios (rápidos, sin clúster)

Verifican la lógica interna: clasificador de cgroups, parser de paths, lógica de métricas Prometheus, validación de parámetros del evaluador y el endpoint HTTP del collector.

```bash
cd ~/tfg/tests
./run_tests.sh
```

Esto ejecuta:
- `cgroup_test.go` — clasificación y parser de paths cgroupfs.
- `prom_metrics_test.go` — delta tracking de counters y agregación por pod_uid.
- `evaluator_test.go` — validación de PromQL y respuestas de Prometheus.
- `collector_http_test.go` — endpoint `/metrics` real con httptest.

Duración: **< 1 segundo**.

### Nivel B — Tests del operator con envtest

Lanzan un control plane efímero de Kubernetes (etcd + kube-apiserver) y verifican que el reconciler responde correctamente.

```bash
cd ~/tfg/operator
make test
```

La primera vez descarga `setup-envtest` y los binarios de Kubernetes (~80 MB). Duración: **~10 segundos**.

### Nivel C — Test end-to-end en un clúster real

**Verifica el ciclo completo** con pods reales saturando CPU e I/O y observando que el operator los remedia.

**Antes de lanzarlo asegúrate de tener:**
- El sistema base corriendo (paso 1.2).
- El operator desplegado (paso 1.3).
- Un port-forward de Prometheus si está dentro del clúster:
  ```bash
  kubectl port-forward svc/prometheus 9090:9090
  ```

Lanzar:

```bash
cd ~/tfg
./tests/e2e/run_e2e.sh         # CPU + IO + migración, ~15 min
./tests/e2e/run_e2e.sh cpu     # solo escenario CPU, ~5 min
./tests/e2e/run_e2e.sh io      # solo escenario IO, ~5 min
./tests/e2e/run_e2e.sh migrate # solo escenario de migración (requiere USB), ~5 min
```

El script:
1. Despliega un pod víctima (`stress-ng` o `fio`).
2. Espera a que la métrica eBPF llegue a Prometheus.
3. Aplica la política inicial (`ScaleUp` o `MigrateStorageClass`).
4. Espera 2 min y verifica que la acción se ejecutó.
5. Cambia la política a `EvictAndTaint` y vuelve a verificar.
6. Limpia todos los recursos creados (vía `trap EXIT`).

Cada escenario imprime su progreso por terminal con prefijos `[INFO]/[OK]/[WARN]/[ERR]`
(ver la sección 3), de modo que se puede seguir qué acción de remediación se está
probando y su resultado sin mirar los logs del operator.

#### Escenario de migración (`migrate`) — requiere un disco lento real

La acción `MigrateStorageClass` mueve el volumen de un pod de una StorageClass
lenta a una rápida. Para probarla de forma realista se usa una **memoria USB
externa como disco lento** (su latencia de I/O es de decenas/cientos de ms,
frente a los ~0,01 ms de un NVMe, que nunca dispararía el umbral).

**Montaje previo (una sola vez):**

```bash
# 1. Formatear la USB a ext4 y montarla en una ruta fija (BORRA la USB)
sudo umount /dev/sda 2>/dev/null || true
sudo mkfs.ext4 -F -L slow-usb /dev/sda
sudo mkdir -p /mnt/slow-usb && sudo mount /dev/sda /mnt/slow-usb
sudo chmod 777 /mnt/slow-usb

# 2. Desplegar los aprovisionadores y las StorageClasses (fast-nvme + slow-usb)
kubectl apply -f monitoring/storage-migration/usb-provisioner.yaml
kubectl apply -f monitoring/storage-migration/storage-classes.yaml
```

> Sustituye `/dev/sda` por el dispositivo real de tu USB (compruébalo con
> `lsblk -o NAME,SIZE,TRAN,MOUNTPOINT`). El aprovisionador `slow-usb` escribe en
> `/mnt/slow-usb`, así que la USB debe estar montada ahí.

El escenario ejecuta **dos casos** que demuestran las dos ramas de la acción:

| Caso | Volumen | Comportamiento esperado |
|------|---------|-------------------------|
| **A** | Solo lectura (`readOnly`) | El operator **migra** el PVC de `slow-usb` a `fast-nvme`: crea un PVC nuevo (`*-migrated-*`) y repunta el Deployment. `Progressing=Remediating`. |
| **B** | Lectura/escritura (`fio` escribiendo) | El operator **rechaza** la migración para no perder datos (no copia datos). `Degraded`; el PVC original permanece intacto en `slow-usb`. |

La lógica es: el operator solo migra cuando el volumen se usa en **solo lectura**
(a nivel de PVC `ReadOnlyMany`, o todos los `volumeMounts` de los containers
principales con `readOnly: true`); si es escribible, aborta de forma segura.

---

## 3. Ver e interpretar los resultados

### Tests unitarios y de operator

La salida de Go usa el formato estándar:

```
=== RUN   TestClassifyCgroupPath
--- PASS: TestClassifyCgroupPath (0.00s)
=== RUN   TestPromCounterDeltaTracking
--- PASS: TestPromCounterDeltaTracking (0.00s)
...
PASS
ok  	tfg/tests	0.021s
```

Significa:
- `--- PASS` (verde) → el caso funcionó como se esperaba.
- `--- FAIL` (rojo) → fallo; debajo aparece el mensaje y la línea exacta del `.go` donde se rompió.
- `ok    paquete    Xs` al final → **todos los tests del paquete pasaron**.

Cualquier `FAIL` rompe el código de salida → `echo $?` devuelve `1`.

Para ver detalle extendido:
```bash
go test ./... -v -run TestClassifyCgroupPath
```

### Test end-to-end (`run_e2e.sh`)

El script usa códigos de color y prefijos claros:

| Prefijo | Color | Significado |
|---------|-------|-------------|
| `[OK]`   | verde | Verificación correcta |
| `[INFO]` | azul  | Información de progreso |
| `[WARN]` | amarillo | Verificación no concluyente (suele ser timing — aumenta `PHASE_DURATION`) |
| `[ERR]`  | rojo  | Fallo de prerrequisitos o conexión |

Ejemplo de ejecución exitosa del escenario CPU:

```
==================================================
[INFO]  ESCENARIO CPU: saturación de Run Queue
==================================================
[INFO]  Desplegando pod víctima (stress-ng, 4 workers, límite 200m)
[OK]    Pod victim-cpu Running
[OK]    Métrica detectada: RunQ latency rate del collector eBPF
==================================================
[INFO]  FASE A — Aplicando política ScaleUp
==================================================
[OK]    ScaleUp ejecutado: 200m → 300m
[OK]    Policy condition Progressing.reason=Remediating
==================================================
[INFO]  FASE B — Sustituyendo política por EvictAndTaint
==================================================
[OK]    Pod victim-cpu fue expulsado (ya no existe)
[OK]    Taint aplicado al nodo nodo-1
```

### Inspección manual durante el test

Mientras `run_e2e.sh` corre puedes abrir otra terminal y observar:

```bash
# Estado del pod víctima y sus resources
kubectl get pod victim-cpu -o jsonpath='{.spec.containers[0].resources}' | jq

# Conditions de la policy
kubectl get ioremediationpolicy -o yaml | grep -A 5 conditions

# Logs del operator (donde estés ejecutando make run)
# Filtra por "Executing action" o "Scaling up"

# Métricas actuales en Prometheus
curl -s 'http://localhost:9090/api/v1/query?query=rate(ebpf_cpu_runq_latency_ns_total[1m])' | jq
```

### Limpieza tras los tests

El test E2E limpia solo. Si interrumpes con Ctrl-C, el `trap EXIT` igualmente:
- Borra los pods víctima.
- Borra las policies aplicadas.
- Quita el taint del nodo.

Para parar el sistema base:
```bash
sudo pkill -f "/collector( |$)"
# Si lanzaste Prometheus con start_system yes
pkill -f "/prometheus( |$)"
# Descargar los programas eBPF (opcional)
sudo rm -rf /sys/fs/bpf/block_latency /sys/fs/bpf/runq_latency
```

---

## Resumen rápido (cheat sheet)

```bash
# ── ARRANCAR ──
sudo ./start_system yes                 # eBPF + collector + Prometheus
cd operator && make install && make run # operator + CRD
cd monitoring && docker compose up -d   # Grafana (http://localhost:3000)

# ── TESTS ──
cd tests && ./run_tests.sh              # unitarios (~1s)
cd operator && make test                # operator + envtest (~10s)
./tests/e2e/run_e2e.sh                  # end-to-end real: cpu+io+migrate (~15min)
./tests/e2e/run_e2e.sh migrate          # solo migración USB→NVMe (requiere USB)

# ── VERIFICAR EN VIVO ──
curl -s http://localhost:9100/metrics | grep ebpf_
kubectl get ioremediationpolicy -o yaml
# Gráficas: http://localhost:3000 (Grafana) o http://localhost:9090 (Prometheus)
```
