#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "=================================================="
echo " Suite de tests del sistema eBPF-Kubernetes"
echo "=================================================="
echo ""

go test ./... -v -count=1 "$@"

echo ""
echo "=================================================="
echo " Todos los tests pasaron correctamente"
echo "=================================================="
