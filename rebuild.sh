#!/bin/bash
# Script para recompilar llama-swap con los últimos cambios

set -e

echo "🔄 Recompilando llama-swap con los últimos cambios..."
cd /home/csolutions_ai/swap-laboratories

echo "📦 Compilando con Docker..."
docker run --rm -v "$PWD":/src -w /src golang:1.25 \
  /usr/local/go/bin/go build -buildvcs=false -o build/llama-swap . || {
  echo "❌ Error en la compilación"
  exit 1
}

echo "✅ Compilación completada"
ls -lh build/llama-swap

echo "🔄 Reiniciando llama-swap..."
pkill -f llama-swap || true
sleep 2
nohup ./build/llama-swap --config /home/csolutions_ai/Swap-Laboratories/config.yaml --watch-config -listen 0.0.0.0:8080 > /tmp/llama-swap.log 2>&1 &
echo "✅ llama-swap reiniciado"

echo "🌐 Verificando servicio..."
sleep 3
curl -s http://localhost:8080/api/version | jq '.' || echo "⚠️ El servicio no está accesible"

echo ""
echo "📋 Cambios aplicados:"
echo "  ✅ Auto-descarga de imagen más reciente al hacer Update"
echo "  ✅ Uso de NGC Catalog para NVIDIA backend"
echo "  ✅ Backend NVIDIA en bloque separado en UI"
echo "  ✅ Detección de actualizaciones disponibles"
