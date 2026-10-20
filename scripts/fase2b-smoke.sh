#!/usr/bin/env bash
# Prueba de humo de la Fase 2b: POST /links de extremo a extremo contra el
# Syncthing real y una Capataz Nube falsa (capataz/cmd/nubefalsa) en
# loopback. Comprueba la preparación asíncrona, la subida por partes, la
# descarga pública descifrada con Web Crypto (scripts/cpk1-verify.mjs), el
# tope de descargas, la revocación y que la bitácora no contenga nombres,
# tokens ni claves.
#
# Usa un estado y un staging propios en un directorio temporal (instanceId
# nuevo), así no toca ~/capataz-host. Crea contenido en
# <carpeta>/_smoke-fase2b/ (con un marcador) y lo borra siempre al salir.
#
# Uso: scripts/fase2b-smoke.sh
# Variables opcionales: SMOKE_FOLDER (fgyww-edjwa),
#   SMOKE_FOLDER_PATH (~/capataz-dev/sync/pruebas), SMOKE_PORT (18082),
#   SMOKE_NUBE_PORT (18090).
#
# El token de desarrollo, la clave de la nube y las claves de enlace nunca se
# imprimen: viven en archivos 0600 del directorio temporal.
set -euo pipefail
set +x

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$ROOT/.env"
FOLDER="${SMOKE_FOLDER:-fgyww-edjwa}"
FOLDER_PATH="${SMOKE_FOLDER_PATH:-$HOME/capataz-dev/sync/pruebas}"
PORT="${SMOKE_PORT:-18082}"
NUBE_PORT="${SMOKE_NUBE_PORT:-18090}"
BASE="http://127.0.0.1:$PORT"
NUBE="http://127.0.0.1:$NUBE_PORT"
SUB="_smoke-fase2b"
SMOKE_DIR="$FOLDER_PATH/$SUB"
MARKER="$SMOKE_DIR/.capataz-smoke"
BIN="$ROOT/capataz/bin/capataz"
VERIFY=(node "$ROOT/scripts/cpk1-verify.mjs")

[[ -f "$ENV_FILE" ]] || { echo "No existe $ENV_FILE" >&2; exit 1; }
[[ -d "$FOLDER_PATH" ]] || { echo "No existe la carpeta de pruebas" >&2; exit 1; }
for cmd in node unzip python3 shasum curl go; do
  command -v "$cmd" >/dev/null || { echo "Falta $cmd" >&2; exit 1; }
done
if [[ -e "$SMOKE_DIR" && ! -f "$MARKER" ]]; then
  echo "$SMOKE_DIR existe y no lo creó este script; no se toca." >&2
  exit 1
fi

env_value() {
  local line
  line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?$1=" "$ENV_FILE" | tail -n 1 || true)"
  line="${line#*=}"
  line="${line#"${line%%[![:space:]]*}"}"; line="${line%"${line##*[![:space:]]}"}"
  if [[ "$line" == \"*\" || "$line" == \'*\' ]]; then line="${line:1:${#line}-2}"; fi
  printf '%s' "$line"
}
TOKEN="$(env_value CAPATAZ_DEV_TOKEN)"
[[ -n "$TOKEN" ]] || { echo "CAPATAZ_DEV_TOKEN vacío en .env" >&2; exit 1; }

umask 077
TMP="$(mktemp -d "${TMPDIR:-/tmp}/capataz-smoke2b.XXXXXX")"
LOG="$TMP/capataz.log"
NUBE_LOG="$TMP/nube.log"
AUTH="$TMP/auth-header"
printf 'Authorization: Bearer %s\n' "$TOKEN" >"$AUTH"
AUTHED=(-H "@$AUTH")
SERVER_PID=""
NUBE_PID=""

stop() { [[ -n "$1" ]] && kill -0 "$1" 2>/dev/null && { kill "$1" 2>/dev/null || true; wait "$1" 2>/dev/null || true; }; return 0; }
cleanup() {
  stop "$SERVER_PID"
  stop "$NUBE_PID"
  [[ -f "$MARKER" ]] && rm -rf "$SMOKE_DIR"
  rm -rf "$TMP"
}
trap cleanup EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  OK     %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FALLO  %s\n' "$1"; [[ -n "${2:-}" ]] && printf '         %s\n' "$2"; return 0; }
sha()  { shasum -a 256 "$1" | cut -c1-64; }

req() {
  local method="$1" url="$2"; shift 2
  STATUS="$(curl -sS -o "$TMP/b" -D "$TMP/h" -w '%{http_code}' -X "$method" "$@" "$url")"
}
json() { python3 -I -c "import json,sys; d=json.load(open(sys.argv[1])); print($1)" "$TMP/b" 2>/dev/null || true; }
post_link() { req POST "$BASE/links" "${AUTHED[@]}" -H 'Content-Type: application/json' --data-binary "$1"; }

expect_error() {
  local name="$1" want_status="$2" want_code="$3" code
  code="$(json 'd.get("code","") if isinstance(d,dict) and set(d)=={"code","message"} else "<no es Error>"')"
  if [[ "$STATUS" == "$want_status" && "$code" == "$want_code" ]]; then
    ok "$name -> $STATUS $code"
  else
    fail "$name" "quería $want_status $want_code; llegó $STATUS ${code:-<sin JSON>}"
  fi
}

# wait_link ID: consulta GET /links/ID hasta que salga de preparing/uploading.
wait_link() {
  local st=""
  for _ in $(seq 1 600); do
    req GET "$BASE/links/$1" "${AUTHED[@]}"
    st="$(json 'd["status"]')"
    [[ "$st" != preparing && "$st" != uploading ]] && break
    sleep 0.1
  done
  printf '%s' "$st"
}

# split_url NOMBRE: separa la url del último cuerpo en $TMP/NOMBRE.token y
# $TMP/NOMBRE.key (0600), sin imprimirlas.
split_url() {
  python3 -I - "$TMP/b" "$TMP/$1" <<'EOF'
import json, sys, urllib.parse
u = urllib.parse.urlsplit(json.load(open(sys.argv[1]))["url"])
open(sys.argv[2] + ".token", "w").write(u.path.removeprefix("/s/"))
open(sys.argv[2] + ".key", "w").write(u.fragment)
EOF
}

# fetch NOMBRE: baja meta y blob de la nube y descifra con Node como el
# navegador; deja el claro en $TMP/NOMBRE.claro.
fetch() {
  local tok
  tok="$(cat "$TMP/$1.token")"
  curl -sS -f -o "$TMP/$1.meta" "$NUBE/s/$tok/meta" || return 1
  python3 -I -c 'import json,sys; open(sys.argv[2],"w").write(json.load(open(sys.argv[1]))["metaBlob"])' "$TMP/$1.meta" "$TMP/$1.meta.b64"
  curl -sS -f -o "$TMP/$1.cpk1" "$NUBE/s/$tok/blob" || return 1
  "${VERIFY[@]}" --package "$TMP/$1.cpk1" --key-file "$TMP/$1.key" --meta "$TMP/$1.meta.b64" \
    --out "$TMP/$1.claro" --meta-out "$TMP/$1.meta.json" >"$TMP/$1.verify" 2>&1
}
public_status() { curl -sS -o /dev/null -w '%{http_code}' "$NUBE/s/$(cat "$TMP/$1.token")/$2"; }

# ---------------------------------------------------------------------------
echo "== Compilando y arrancando la nube falsa ($NUBE) y Capataz ($BASE)"
make -s -C "$ROOT/capataz" build >/dev/null
(cd "$ROOT/capataz" && go build -o "$TMP/nubefalsa" ./cmd/nubefalsa)
mkdir -p "$TMP/state" "$TMP/staging"
INSTANCE="$(python3 -I -c 'import base64,os; print(base64.b32encode(os.urandom(16)).decode().rstrip("=").lower())')"
printf '{"instanceId":"%s","createdAt":"2026-01-01T00:00:00Z"}\n' "$INSTANCE" >"$TMP/state/instance.json"
python3 -I -c 'import base64,os,sys; open(sys.argv[1],"w").write(base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("="))' "$TMP/nube.key"

"$TMP/nubefalsa" --listen "127.0.0.1:$NUBE_PORT" --instance "$INSTANCE" --key-file "$TMP/nube.key" >"$NUBE_LOG" 2>&1 &
NUBE_PID=$!
for _ in $(seq 1 50); do
  curl -s -o /dev/null "$NUBE/s/x/meta" && break
  kill -0 "$NUBE_PID" 2>/dev/null || { echo "La nube falsa no arrancó:" >&2; cat "$NUBE_LOG" >&2; exit 1; }
  sleep 0.1
done
# El entorno real gana sobre el .env: puerto, estado y staging propios.
CAPATAZ_LISTEN="127.0.0.1:$PORT" CAPATAZ_STATE_DIR="$TMP/state" CAPATAZ_STAGING_DIR="$TMP/staging" \
  CAPATAZ_NUBE_URL="$NUBE" CAPATAZ_NUBE_KEY="$(cat "$TMP/nube.key")" \
  "$BIN" -env-file "$ENV_FILE" >"$LOG" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -s -o /dev/null "$BASE/instance" && break
  kill -0 "$SERVER_PID" 2>/dev/null || { echo "Capataz no arrancó:" >&2; cat "$LOG" >&2; exit 1; }
  sleep 0.1
done
grep -q "Capataz Nube conectada" "$LOG" && ok "Capataz firmó GET /v1/instance y la nube lo aceptó" \
  || fail "la nube no aceptó la firma" "$(grep -i nube "$LOG" | tail -n 1)"

echo "== Contenido de prueba en <carpeta>/$SUB"
mkdir -p "$SMOKE_DIR/dir-smoke2b/sub"
: >"$MARKER"
head -c $((9 * 1024 * 1024 + 7)) /dev/urandom >"$SMOKE_DIR/grande.bin"
for i in $(seq 1 200); do echo "línea $i"; done >"$SMOKE_DIR/dir-smoke2b/notas.txt"
echo "# documento" >"$SMOKE_DIR/dir-smoke2b/sub/doc.md"
ok "creado: 9 MiB + 7 bytes (dos partes de 8 MiB) y una carpeta con 2 archivos"

# ---------------------------------------------------------------------------
echo "== Validación síncrona"
req POST "$BASE/links" -H 'Content-Type: application/json' --data-binary '{}'; expect_error "POST /links sin token" 401 unauthorized
req GET "$BASE/links" "${AUTHED[@]}"
[[ "$STATUS" == 200 && "$(json 'len(d)')" == 0 ]] && ok "GET /links -> 200 []" || fail "GET /links vacío" "estado $STATUS"
post_link "{\"items\":[{\"folder\":\"$FOLDER\",\"path\":\"$SUB/grande.bin\"}],\"encryption\":\"none\"}"; expect_error "encryption=none" 400 invalid_request
post_link "{\"items\":[{\"folder\":\"$FOLDER\",\"path\":\"$SUB/grande.bin\"}],\"ttlSeconds\":60}";     expect_error "ttlSeconds=60" 400 invalid_request
post_link "{\"items\":[{\"folder\":\"$FOLDER\",\"path\":\"../fuera\"}]}";                              expect_error "ruta con ../" 400 path_not_allowed
post_link "{\"items\":[{\"folder\":\"$FOLDER\",\"path\":\"$SUB/no-existe\"}]}";                        expect_error "ruta inexistente" 404 not_found
post_link "{\"items\":[{\"folder\":\"no-existe\",\"path\":\"x\"}]}";                                   expect_error "carpeta desconocida" 404 not_found
post_link "{\"items\":[],\"otro\":1}";                                                                 expect_error "campo desconocido" 400 invalid_request

echo "== Archivo de 9 MiB: preparing -> active, subida en 2 partes"
post_link "{\"items\":[{\"folder\":\"$FOLDER\",\"path\":\"$SUB/grande.bin\"}],\"ttlSeconds\":3600}"
A="$(json 'd["id"]')"
[[ "$STATUS" == 202 && "$(json '(d["status"], d["url"], d["encryption"])')" == "('preparing', None, 'client')" ]] \
  && ok "POST /links -> 202 preparing, sin url, encryption=client" || fail "POST /links" "estado $STATUS"
st="$(wait_link "$A")"
if [[ "$st" == active ]]; then
  ok "GET /links/{id} -> active"
  [[ "$(python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["url"].startswith(sys.argv[2]+"/s/") and len(d["url"].split("#")[1])==43)' "$TMP/b" "$NUBE")" == True ]] \
    && ok "url = nube/s/<token>#<clave de 43 caracteres>" || fail "formato de la url"
  [[ "$(json '(d["progress"], d["packageSize"] == 32 + d["payloadSize"] + 16*10)')" == "(1, True)" ]] \
    && ok "progress = 1 y packageSize = 32 + largo + 16·10 bloques" || fail "progreso o tamaños"
  [[ "$(json 'd["payloadSha256"]')" == "$(sha "$SMOKE_DIR/grande.bin")" ]] && ok "payloadSha256 = SHA-256 del original" || fail "payloadSha256"
  split_url a
  if fetch a; then
    [[ "$(sha "$TMP/a.claro")" == "$(sha "$SMOKE_DIR/grande.bin")" ]] \
      && ok "descargado de la nube y descifrado en Node: idéntico al original" || fail "SHA-256 del descifrado distinto"
    [[ "$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$TMP/a.meta.json")" == grande.bin ]] \
      && ok "meta.name descifrado correcto" || fail "meta.name"
  else
    fail "descargar o descifrar" "$(tail -n 2 "$TMP/a.verify" 2>/dev/null | tr '\n' ' ')"
  fi
else
  fail "el enlace no llegó a active" "estado $st, failureCode $(json 'd.get("failureCode")')"
fi
[[ -z "$(ls "$TMP/staging")" ]] && ok "staging vacío tras activar (el paquete se borra)" || fail "quedó algo en el staging"

echo "== Carpeta (ZIP) con maxDownloads=1"
post_link "{\"items\":[{\"folder\":\"$FOLDER\",\"path\":\"$SUB/dir-smoke2b\"}],\"maxDownloads\":1,\"label\":\"Etiqueta smoke\"}"
B="$(json 'd["id"]')"
st="$(wait_link "$B")"
if [[ "$st" == active ]]; then
  split_url b
  if fetch b; then
    unzip -tq "$TMP/b.claro" >/dev/null 2>&1 && ok "ZIP descargado, descifrado e íntegro (unzip -t)" || fail "unzip -t"
    [[ "$(public_status b blob)" == 410 ]] && ok "segunda descarga -> 410 (cupo agotado)" || fail "el cupo no se aplicó"
  else
    fail "descargar o descifrar el ZIP" "$(tail -n 2 "$TMP/b.verify" 2>/dev/null | tr '\n' ' ')"
  fi
else
  fail "el ZIP no llegó a active" "estado $st, failureCode $(json 'd.get("failureCode")')"
fi

echo "== Revocar"
req DELETE "$BASE/links/$A" "${AUTHED[@]}"
[[ "$STATUS" == 204 ]] && ok "DELETE /links/{id} -> 204" || fail "DELETE" "estado $STATUS"
req GET "$BASE/links/$A" "${AUTHED[@]}"
[[ "$(json '(d["status"], d["url"])')" == "('revoked', None)" ]] && ok "status revoked y sin url" || fail "estado tras revocar"
[[ "$(public_status a meta)" == 410 ]] && ok "la nube responde 410 en /s/{token}/meta" || fail "la nube sigue sirviendo el enlace"
req DELETE "$BASE/links/$A" "${AUTHED[@]}"
[[ "$STATUS" == 204 ]] && ok "revocar otra vez -> 204 (idempotente)" || fail "segunda revocación" "estado $STATUS"
req DELETE "$BASE/links/no-existe-000" "${AUTHED[@]}"; expect_error "revocar inexistente" 404 not_found
req GET "$BASE/links?status=active" "${AUTHED[@]}"
[[ "$(json 'len(d)')" == 1 ]] && ok "GET /links?status=active -> 1" || fail "filtro por estado" "$(json 'len(d)')"

# ---------------------------------------------------------------------------
echo "== Privacidad"
python3 -I - "$TMP/state/links" <<'EOF' && ok "registros de enlace 0600 en un directorio 0700" || fail "permisos del estado de enlaces"
import os, stat, sys
d = sys.argv[1]
ok = stat.S_IMODE(os.stat(d).st_mode) == 0o700
ok &= all(stat.S_IMODE(os.stat(os.path.join(d, f)).st_mode) == 0o600 for f in os.listdir(d))
sys.exit(0 if ok else 1)
EOF
leaks=""
# Son nombres sintéticos de esta prueba: se pueden mostrar si aparecen.
for f in grande.bin dir-smoke2b notas.txt doc.md "$SUB" "Etiqueta smoke"; do grep -qF -- "$f" "$LOG" "$NUBE_LOG" && leaks="$leaks $f"; done
[[ -z "$leaks" ]] && ok "ningún nombre, ruta ni etiqueta en la bitácora de Capataz ni de la nube" || fail "la bitácora contiene nombres o rutas:$leaks"
leaks=0
for s in "$TOKEN" "$(cat "$TMP/nube.key")" "$(cat "$TMP/a.token" 2>/dev/null)" "$(cat "$TMP/a.key" 2>/dev/null)" "$(cat "$TMP/b.key" 2>/dev/null)"; do
  [[ -n "$s" ]] && grep -qF -- "$s" "$LOG" "$NUBE_LOG" && leaks=$((leaks + 1))
done
[[ "$leaks" == 0 ]] && ok "ningún token ni clave en la bitácora" || fail "la bitácora contiene $leaks secretos"
grep -q "enlace solicitado.*owner=" "$LOG" && grep -q "enlace activo" "$LOG" && ok "la bitácora registra quién y qué enlace (solicitado/activo/terminado)" || fail "faltan eventos de enlace en la bitácora"

echo
echo "== Resultado: $PASS OK, $FAIL fallos (la carpeta $SUB y el estado temporal se borran al salir)"
[[ "$FAIL" == 0 ]]
