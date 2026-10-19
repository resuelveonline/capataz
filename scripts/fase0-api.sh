#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; source .env; set +a

REF="${1:-pruebas}"     # etiqueta o ID de la carpeta
FILE="${2:-uno.txt}"
api() { curl -s -H "X-API-Key: $SYNCTHING_API_KEY" "$SYNCTHING_URL/rest/$1"; }
show() {
  local out; out=$(api "$1")
  echo "$out" | jq "${2:-.}" 2>/dev/null || echo "RESPUESTA NO JSON: $out"
}

FOLDER=$(api config/folders | jq -r --arg r "$REF" '.[] | select(.id==$r or .label==$r) | .id' | head -n1)
if [ -z "$FOLDER" ]; then echo "No encontré la carpeta '$REF'"; exit 1; fi
echo "Carpeta: $REF -> id $FOLDER"

echo "### system/version";  show system/version '{version, os, arch}'
echo "### config/folders";  show config/folders '[.[] | {id, label, path, type}]'
echo "### db/status";       show "db/status?folder=$FOLDER" '{state, globalFiles, localFiles, needFiles, needBytes}'
echo "### db/browse";       show "db/browse?folder=$FOLDER&levels=1"
echo "### db/file";         show "db/file?folder=$FOLDER&file=$FILE"
echo "### db/need";         show "db/need?folder=$FOLDER"
