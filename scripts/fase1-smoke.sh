#!/usr/bin/env bash
# Prueba de humo de la Fase 1 de Capataz contra el Syncthing local.
#
# Compila Capataz, lo arranca en un puerto aparte con el .env del repo,
# recorre cada endpoint con curl y al final comprueba que la bitácora del
# servidor no contenga nombres de archivo de la carpeta ni el token.
#
# Uso (desde cualquier sitio):  scripts/fase1-smoke.sh
# Variables opcionales:
#   SMOKE_FOLDER       id de la carpeta     (por defecto fgyww-edjwa)
#   SMOKE_LABEL        label esperado       (por defecto pruebas)
#   SMOKE_FOLDER_PATH  ruta en disco        (por defecto ~/capataz-dev/sync/pruebas)
#   SMOKE_PORT         puerto de Capataz    (por defecto 18081)
#
# El token nunca se imprime: viaja en un archivo de headers temporal (0600)
# para que tampoco aparezca en la lista de procesos.
set -euo pipefail
set +x

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$ROOT/.env"
FOLDER="${SMOKE_FOLDER:-fgyww-edjwa}"
LABEL="${SMOKE_LABEL:-pruebas}"
FOLDER_PATH="${SMOKE_FOLDER_PATH:-$HOME/capataz-dev/sync/pruebas}"
PORT="${SMOKE_PORT:-18081}"
BASE="http://127.0.0.1:$PORT"

[[ -f "$ENV_FILE" ]] || { echo "No existe $ENV_FILE" >&2; exit 1; }

# Lee una variable del .env sin imprimirla (quita comillas envolventes).
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
TMP="$(mktemp -d "${TMPDIR:-/tmp}/capataz-smoke.XXXXXX")"
LOG="$TMP/capataz.log"
AUTH="$TMP/auth-header"
printf 'Authorization: Bearer %s\n' "$TOKEN" > "$AUTH"
AUTHED=(-H "@$AUTH")  # curl lee el header del archivo
SERVER_PID=""

cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  OK     %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FALLO  %s\n' "$1"; [[ -n "${2:-}" ]] && printf '         %s\n' "$2"; return 0; }

# req MÉTODO URL [args de curl...]: deja estado, headers y cuerpo en
# $STATUS, $TMP/h y $TMP/b.
req() {
  local method="$1" url="$2"; shift 2
  local mflag=(-X "$method")
  [[ "$method" == HEAD ]] && mflag=(-I)
  STATUS="$(curl -sS -o "$TMP/b" -D "$TMP/h" -w '%{http_code}' "${mflag[@]}" "$@" "$url")"
}
header() { { grep -i "^$1:" "$TMP/h" || true; } | head -n 1 | cut -d: -f2- | tr -d '\r' | sed 's/^ //'; }
json()   { python3 -I -c "import json,sys; d=json.load(open(sys.argv[1])); print($1)" "$TMP/b" 2>/dev/null || true; }

# expect_error NOMBRE ESTADO CÓDIGO: comprueba estado y cuerpo {code,message}.
expect_error() {
  local name="$1" want_status="$2" want_code="$3" code ctype
  code="$(json 'd.get("code","") if isinstance(d,dict) and set(d)=={"code","message"} else "<no es Error>"')"
  ctype="$(header Content-Type)"
  if [[ "$STATUS" == "$want_status" && "$code" == "$want_code" && "$ctype" == application/json* ]]; then
    ok "$name -> $STATUS $code"
  else
    fail "$name" "quería $want_status $want_code; llegó $STATUS ${code:-<sin JSON>} ($ctype)"
  fi
}

expect_status() {
  if [[ "$STATUS" == "$2" ]]; then ok "$1 -> $STATUS"; else fail "$1" "quería $2; llegó $STATUS"; fi
}

# ---------------------------------------------------------------------------
echo "== Compilando y arrancando Capataz en $BASE"
make -s -C "$ROOT/capataz" build >/dev/null
# El entorno real gana sobre el .env: así el puerto no choca con un
# Capataz de desarrollo ya en marcha.
CAPATAZ_LISTEN="127.0.0.1:$PORT" "$ROOT/capataz/bin/capataz" -env-file "$ENV_FILE" >"$LOG" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -s -o /dev/null "$BASE/instance" && break
  kill -0 "$SERVER_PID" 2>/dev/null || { echo "Capataz no arrancó:" >&2; cat "$LOG" >&2; exit 1; }
  sleep 0.1
done

echo "== /instance"
req GET "$BASE/instance"
if [[ "$STATUS" == 200 && -n "$(json 'd["instanceId"]')" && "$(json 'd["publicMode"]')" == local ]]; then
  ok "GET /instance sin token -> 200 (instanceId $(json 'd["instanceId"]'), versión $(json 'd["version"]'))"
else
  fail "GET /instance" "estado $STATUS"
fi

echo "== Autenticación"
req GET "$BASE/folders";                                               expect_error "GET /folders sin token" 401 unauthorized
[[ "$(header WWW-Authenticate)" == Bearer ]] && ok "WWW-Authenticate: Bearer" || fail "falta WWW-Authenticate: Bearer"
req GET "$BASE/folders" -H "Authorization: Bearer token-incorrecto-0000000000000000"; expect_error "GET /folders token incorrecto" 401 unauthorized
# Basic con el token correcto: el esquema también cuenta.
req GET "$BASE/folders" -H @<(printf 'Authorization: Basic %s\n' "$TOKEN"); expect_error "GET /folders esquema Basic" 401 unauthorized
req HEAD "$BASE/files/download?folder=$FOLDER&path=x";                 expect_status "HEAD /files/download sin token" 401

echo "== /folders"
req GET "$BASE/folders" "${AUTHED[@]}"
if [[ "$STATUS" == 200 && "$(json "next((f['label'] for f in d if f['id']=='$FOLDER'),'')")" == "$LABEL" ]]; then
  ok "GET /folders -> 200 con id $FOLDER y label \"$LABEL\""
else
  fail "GET /folders" "estado $STATUS; no aparece $FOLDER con label $LABEL"
fi
[[ "$(json "sorted(set(k for f in d for k in f))")" == "['id', 'label']" ]] && ok "Folder solo expone id y label" || fail "Folder expone campos de más"

echo "== /files"
req GET "$BASE/files?folder=$FOLDER" "${AUTHED[@]}"
expect_status "GET /files raíz" 200
NAMES="$(json "'\n'.join(e['name'] for e in d)")"
FILE="$(json "next((e['path'] for e in d if not e['isDir'] and e['size']>=4),'')")"
DIR="$(json "next((e['path'] for e in d if e['isDir']),'')")"
SIZE="$(json "next((e['size'] for e in d if e['path']=='$FILE'),'')")"
[[ "$(json "sorted(set(k for e in d for k in e))")" == "['isDir', 'modified', 'name', 'path', 'size']" ]] \
  && ok "Entry con name, path, isDir, size, modified" || fail "Entry con campos inesperados"
if grep -qiE '^(\.stfolder|\.stversions|\.stignore|\.syncthing\..*\.tmp|~syncthing~.*\.tmp)$' <<<"$NAMES"; then
  fail "el listado muestra archivos internos de Syncthing"
else
  ok "el listado oculta .stfolder, .stversions, .stignore y temporales"
fi
[[ -n "$FILE" ]] || { echo "La carpeta necesita al menos un archivo de 4 bytes o más en la raíz" >&2; exit 1; }
echo "     (archivo de prueba: $SIZE bytes; subcarpeta de prueba: $([[ -n "$DIR" ]] && echo sí || echo no))"

if [[ -n "$DIR" ]]; then
  req GET "$BASE/files" -G --data-urlencode "folder=$FOLDER" --data-urlencode "path=$DIR" "${AUTHED[@]}"
  expect_status "GET /files subcarpeta" 200
fi
req GET "$BASE/files" -G --data-urlencode "folder=$FOLDER" --data-urlencode "path=$FILE" "${AUTHED[@]}"; expect_error "GET /files sobre un archivo" 400 invalid_request
req GET "$BASE/files?path=x" "${AUTHED[@]}";                                    expect_error "GET /files sin folder" 400 invalid_request
req GET "$BASE/files?folder=no-existe" "${AUTHED[@]}";                          expect_error "GET /files carpeta inexistente" 404 not_found
req GET "$BASE/files?folder=$FOLDER&path=no-existe-capataz" "${AUTHED[@]}";     expect_error "GET /files ruta inexistente" 404 not_found
req GET "$BASE/files?folder=$FOLDER&path=.stfolder" "${AUTHED[@]}";             expect_error "GET /files .stfolder" 404 not_found
req GET "$BASE/files?folder=$FOLDER&path=../" "${AUTHED[@]}";                   expect_error "GET /files ../" 400 path_not_allowed
req GET "$BASE/files?folder=$FOLDER&path=%2e%2e%2f%2e%2e" "${AUTHED[@]}";       expect_error "GET /files %2e%2e%2f codificado" 400 path_not_allowed
req GET "$BASE/files?folder=$FOLDER&path=%2Fetc" "${AUTHED[@]}";                expect_error "GET /files ruta absoluta" 400 path_not_allowed
req GET "$BASE/files?folder=$FOLDER&path=a%00b" "${AUTHED[@]}";                 expect_error "GET /files byte nulo" 400 path_not_allowed

echo "== /files/download"
DL=(-G --data-urlencode "folder=$FOLDER" --data-urlencode "path=$FILE")
req GET "$BASE/files/download" "${DL[@]}" "${AUTHED[@]}"
expect_status "descarga completa" 200
if [[ -f "$FOLDER_PATH/$FILE" ]]; then
  cmp -s "$TMP/b" "$FOLDER_PATH/$FILE" && ok "contenido idéntico al archivo en disco" || fail "el contenido no coincide con el disco"
else
  [[ "$(wc -c <"$TMP/b" | tr -d ' ')" == "$SIZE" ]] && ok "tamaño correcto ($SIZE bytes)" || fail "tamaño incorrecto"
fi
[[ "$(header Content-Type)" == application/octet-stream ]] && ok "Content-Type: application/octet-stream" || fail "Content-Type $(header Content-Type)"
[[ "$(header Content-Disposition)" == attachment* ]] && ok "Content-Disposition: attachment" || fail "falta Content-Disposition"
[[ "$(header Accept-Ranges)" == bytes ]] && ok "Accept-Ranges: bytes" || fail "falta Accept-Ranges"
LAST_MOD="$(header Last-Modified)"

req HEAD "$BASE/files/download" "${DL[@]}" "${AUTHED[@]}"
[[ "$STATUS" == 200 && "$(header Content-Length)" == "$SIZE" ]] && ok "HEAD con token -> 200, Content-Length $SIZE" || fail "HEAD con token" "estado $STATUS"

req GET "$BASE/files/download" "${DL[@]}" "${AUTHED[@]}" -H "Range: bytes=0-2"
[[ "$STATUS" == 206 && "$(header Content-Range)" == "bytes 0-2/$SIZE" && "$(wc -c <"$TMP/b" | tr -d ' ')" == 3 ]] \
  && ok "Range bytes=0-2 -> 206, Content-Range bytes 0-2/$SIZE" || fail "Range 0-2" "estado $STATUS, Content-Range $(header Content-Range)"

req GET "$BASE/files/download" "${DL[@]}" "${AUTHED[@]}" -H "Range: bytes=3-" -H "If-Range: $LAST_MOD"
[[ "$STATUS" == 206 && "$(header Content-Range)" == "bytes 3-$((SIZE - 1))/$SIZE" ]] \
  && ok "reanudación Range bytes=3- + If-Range -> 206" || fail "reanudación" "estado $STATUS"

req GET "$BASE/files/download" "${DL[@]}" "${AUTHED[@]}" -H "Range: bytes=$((SIZE + 100))-"
expect_error "Range fuera del archivo" 416 invalid_request
[[ "$(header Content-Range)" == "bytes */$SIZE" ]] && ok "416 con Content-Range: bytes */$SIZE" || fail "416 sin Content-Range correcto" "$(header Content-Range)"

if [[ -n "$DIR" ]]; then
  req GET "$BASE/files/download" -G --data-urlencode "folder=$FOLDER" --data-urlencode "path=$DIR" "${AUTHED[@]}"
  expect_error "descarga de una carpeta" 400 invalid_request
fi
req GET "$BASE/files/download?folder=$FOLDER" "${AUTHED[@]}";                  expect_error "descarga sin path" 400 invalid_request
req GET "$BASE/files/download?folder=$FOLDER&path=.stfolder" "${AUTHED[@]}";   expect_error "descarga .stfolder" 404 not_found
req GET "$BASE/files/download?folder=$FOLDER&path=.stignore" "${AUTHED[@]}";   expect_error "descarga .stignore" 404 not_found
req GET "$BASE/files/download?folder=$FOLDER&path=..%2F..%2Fetc%2Fhosts" "${AUTHED[@]}"; expect_error "descarga ../ codificado" 400 path_not_allowed
req GET "$BASE/files/download?folder=no-existe&path=x" "${AUTHED[@]}";         expect_error "descarga carpeta inexistente" 404 not_found

echo "== Enrutador"
req POST "$BASE/folders" "${AUTHED[@]}";       expect_error "POST /folders" 405 invalid_request
req GET "$BASE/no-existe" "${AUTHED[@]}";      expect_error "ruta inexistente" 404 not_found

# ---------------------------------------------------------------------------
echo "== Privacidad de la bitácora"
kill "$SERVER_PID"; wait "$SERVER_PID" 2>/dev/null || true; SERVER_PID=""

LINES="$(grep -c 'msg=petición' "$LOG" || true)"
grep -q 'route="GET /files/download"' "$LOG" && ok "la bitácora registra las peticiones ($LINES líneas con patrón de ruta)" \
  || fail "la bitácora no registró las peticiones"

# Nombres a buscar: los fijos de "pruebas" más todo lo que hay en la raíz.
LEAK=0
while IFS= read -r name; do
  [[ -z "$name" ]] && continue
  if grep -qF -- "$name" "$LOG"; then fail "la bitácora contiene el nombre \"$name\""; LEAK=1; fi
done < <({ printf "%s\n" uno.txt nuevo.txt subcarpeta; printf "%s\n" "$NAMES"; } | sort -u)
[[ "$LEAK" == 0 ]] && ok "ningún nombre de archivo de la carpeta en la bitácora"

if grep -qF -- "$TOKEN" "$LOG"; then fail "la bitácora contiene el token"; else ok "el token no aparece en la bitácora"; fi
LEAK=0
for pat in 'path=' 'folder=' 'Bearer' 'Basic' '?'; do
  if grep -qF -- "$pat" "$LOG"; then fail "la bitácora contiene \"$pat\""; LEAK=1; fi
done
[[ "$LEAK" == 0 ]] && ok "sin cadenas de consulta ni headers en la bitácora"

echo
echo "== Resultado: $PASS OK, $FAIL fallos"
[[ "$FAIL" == 0 ]]
