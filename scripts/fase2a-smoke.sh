#!/usr/bin/env bash
# Prueba de humo de la Fase 2a: copia congelada, ZIP y cifrado CPK1, todo en
# local y contra el Syncthing real, sin red ni Capataz Nube.
#
# Crea contenido de prueba en <carpeta>/_smoke-fase2a/ (con un marcador) y lo
# borra siempre al terminar, aunque falle. Por cada caso ejecuta
# `capataz prepare`, descifra con scripts/cpk1-verify.mjs (Web Crypto, como
# el navegador) y compara SHA-256 con el original; los ZIP se prueban además
# con `unzip -t`. Al final comprueba que la bitácora no contenga nombres ni
# claves y que el staging quede vacío.
#
# Uso: scripts/fase2a-smoke.sh
# Variables opcionales: SMOKE_FOLDER (fgyww-edjwa),
#   SMOKE_FOLDER_PATH (~/capataz-dev/sync/pruebas).
#
# Las claves nunca se imprimen: van a archivos 0600 en un directorio temporal.
set -euo pipefail
set +x

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$ROOT/.env"
FOLDER="${SMOKE_FOLDER:-fgyww-edjwa}"
FOLDER_PATH="${SMOKE_FOLDER_PATH:-$HOME/capataz-dev/sync/pruebas}"
SUB="_smoke-fase2a"
SMOKE_DIR="$FOLDER_PATH/$SUB"
MARKER="$SMOKE_DIR/.capataz-smoke"
BIN="$ROOT/capataz/bin/capataz"
VERIFY=(node "$ROOT/scripts/cpk1-verify.mjs")

[[ -f "$ENV_FILE" ]] || { echo "No existe $ENV_FILE" >&2; exit 1; }
[[ -d "$FOLDER_PATH" ]] || { echo "No existe la carpeta de pruebas" >&2; exit 1; }
for cmd in node unzip python3 shasum; do
  command -v "$cmd" >/dev/null || { echo "Falta $cmd" >&2; exit 1; }
done

env_value() {
  local line
  line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?$1=" "$ENV_FILE" | tail -n 1 || true)"
  line="${line#*=}"
  line="${line#"${line%%[![:space:]]*}"}"; line="${line%"${line##*[![:space:]]}"}"
  if [[ "$line" == \"*\" || "$line" == \'*\' ]]; then line="${line:1:${#line}-2}"; fi
  printf '%s' "$line"
}
STAGING="$(env_value CAPATAZ_STAGING_DIR)"
STAGING="${STAGING:-~/capataz-host/staging}"
STAGING="${STAGING/#\~/$HOME}"

if [[ -e "$SMOKE_DIR" && ! -f "$MARKER" ]]; then
  echo "$SMOKE_DIR existe y no lo creó este script; no se toca." >&2
  exit 1
fi

umask 077
TMP="$(mktemp -d "${TMPDIR:-/tmp}/capataz-smoke2a.XXXXXX")"
LOG="$TMP/prepare.log" # stderr de todas las ejecuciones de prepare
cleanup() {
  [[ -f "$MARKER" ]] && rm -rf "$SMOKE_DIR"
  rm -rf "$TMP"
}
trap cleanup EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  OK     %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FALLO  %s\n' "$1"; [[ -n "${2:-}" ]] && printf '         %s\n' "$2"; return 0; }
sha()  { shasum -a 256 "$1" | cut -c1-64; }
mf()   { python3 -I -c 'import json,sys; print(eval(sys.argv[2], {}, {"m": json.load(open(sys.argv[1]))}))' "$1" "$2"; }

# prepare NOMBRE RUTA... : ejecuta capataz prepare con --print-key; deja la
# salida en $TMP/NOMBRE/ y la clave en $TMP/NOMBRE.key. Devuelve su código.
prepare() {
  local name="$1"; shift
  local args=()
  for p in "$@"; do args+=(--path "$p"); done
  "$BIN" prepare -env-file "$ENV_FILE" --folder "$FOLDER" "${args[@]}" --out "$TMP/$name" --print-key \
    >"$TMP/$name.key" 2>>"$LOG"
}

# verify NOMBRE: descifra con Node; deja el claro en $TMP/NOMBRE.claro.
verify() {
  "${VERIFY[@]}" --package "$TMP/$1/package.cpk1" --key-file "$TMP/$1.key" --meta "$TMP/$1/meta.b64" \
    --out "$TMP/$1.claro" --meta-out "$TMP/$1.meta.json" >"$TMP/$1.verify" 2>&1
}

# ---------------------------------------------------------------------------
echo "== Preparando contenido de prueba en <carpeta>/$SUB"
make -s -C "$ROOT/capataz" build >/dev/null
mkdir -p "$SMOKE_DIR/carpeta/sub" "$SMOKE_DIR/carpeta/vacia"
: >"$MARKER"
head -c $((3 * 1024 * 1024 + 1)) /dev/urandom >"$SMOKE_DIR/grande.bin"
for i in $(seq 1 300); do echo "línea $i de texto comprimible"; done >"$SMOKE_DIR/carpeta/notas.txt"
head -c 200000 /dev/urandom >"$SMOKE_DIR/carpeta/foto.jpg"
echo "# documento" >"$SMOKE_DIR/carpeta/sub/doc.md"
ln -s notas.txt "$SMOKE_DIR/carpeta/enlace-notas"
NFC="$(printf 'nfc-a\xc3\xb1o.txt')"   # ñ compuesta (U+00F1)
NFD="$(printf 'nfd-an\xcc\x83o.txt')"  # n + tilde combinante (U+0303)
echo "contenido NFC" >"$SMOKE_DIR/$NFC"
echo "contenido NFD" >"$SMOKE_DIR/$NFD"
ok "contenido creado (3 MiB + 1 byte, carpeta con 3 archivos, una vacía y un enlace, nombres NFC y NFD)"

# ---------------------------------------------------------------------------
echo "== Archivo de 3 MiB + 1 byte"
if prepare grande "$SUB/grande.bin" && verify grande; then
  [[ "$(sha "$TMP/grande.claro")" == "$(sha "$SMOKE_DIR/grande.bin")" ]] \
    && ok "descifrado en Node idéntico al original (SHA-256)" || fail "SHA-256 distinto del original"
  size=$((3 * 1024 * 1024 + 1))
  [[ "$(mf "$TMP/grande/manifest.json" 'm["packageSize"]')" == "$((32 + size + 16 * 4))" ]] \
    && ok "packageSize = 32 + largo + 16·4 bloques" || fail "packageSize inesperado"
  [[ "$(mf "$TMP/grande/manifest.json" 'm["kind"], m["payloadSha256"]')" == "('file', '$(sha "$SMOKE_DIR/grande.bin")')" ]] \
    && ok "manifiesto: kind=file y payloadSha256 del original" || fail "manifiesto inesperado"
  [[ "$(mf "$TMP/grande.meta.json" 'm["name"]')" == "grande.bin" ]] && ok "meta.name correcto" || fail "meta.name incorrecto"
else
  fail "preparar o descifrar grande.bin" "$(tail -n 2 "$TMP/grande.verify" "$LOG" 2>/dev/null | tr '\n' ' ')"
fi

echo "== Carpeta con varios archivos (ZIP)"
if prepare carpeta "$SUB/carpeta" && verify carpeta; then
  unzip -tq "$TMP/carpeta.claro" >/dev/null 2>&1 && ok "unzip -t: ZIP íntegro" || fail "unzip -t encontró errores"
  # Compara cada archivo del ZIP con el original, sin imprimir nombres.
  cmp_out="$(python3 -I - "$TMP/carpeta.claro" "$SMOKE_DIR" <<'EOF'
import hashlib, os, sys, zipfile
zf, base = zipfile.ZipFile(sys.argv[1]), sys.argv[2]
names = zf.namelist()
files = [n for n in names if not n.endswith("/")]
bad = sum(hashlib.sha256(zf.read(n)).digest() != hashlib.sha256(open(os.path.join(base, n), "rb").read()).digest() for n in files)
dirs = sorted(n for n in names if n.endswith("/"))
print(len(files), bad, ",".join(dirs) == "carpeta/,carpeta/sub/,carpeta/vacia/", all(n.startswith("carpeta/") for n in names))
EOF
)"
  read -r nfiles nbad dirs_ok prefix_ok <<<"$cmp_out"
  [[ "$nfiles" == 3 && "$nbad" == 0 ]] && ok "los 3 archivos del ZIP son idénticos a los originales" || fail "contenido del ZIP" "$nfiles archivos, $nbad distintos"
  [[ "$dirs_ok" == True && "$prefix_ok" == True ]] && ok "raíz carpeta/ y carpeta vacía incluida" || fail "estructura del ZIP inesperada"
  [[ "$(mf "$TMP/carpeta/manifest.json" 'm["skipped"]["symlink"]')" == 1 ]] && ok "enlace simbólico omitido y contado (skipped.symlink = 1)" || fail "conteo de omitidos"
  [[ "$(mf "$TMP/carpeta.meta.json" 'm["name"], m["entryCount"]')" == "('carpeta.zip', 3)" ]] && ok "meta: carpeta.zip, entryCount = 3 (solo archivos)" || fail "meta del ZIP"
else
  fail "preparar o descifrar la carpeta" "$(tail -n 2 "$TMP/carpeta.verify" "$LOG" 2>/dev/null | tr '\n' ' ')"
fi

echo "== Nombres con ñ en NFC y en NFD"
for form in NFC NFD; do
  name="${!form}"
  if prepare "nombre-$form" "$SUB/$name" && verify "nombre-$form"; then
    [[ "$(sha "$TMP/nombre-$form.claro")" == "$(sha "$SMOKE_DIR/$name")" ]] \
      && ok "$form: preparado y descifrado, SHA-256 idéntico" || fail "$form: SHA-256 distinto"
  else
    fail "$form: no se pudo preparar o descifrar" "$(tail -n 1 "$LOG")"
  fi
done

echo "== Varios elementos"
if prepare varios "$SUB/grande.bin" "$SUB/carpeta" "$SUB/$NFC" && verify varios; then
  unzip -tq "$TMP/varios.claro" >/dev/null 2>&1 && ok "unzip -t: ZIP íntegro" || fail "unzip -t encontró errores"
  [[ "$(mf "$TMP/varios.meta.json" 'm["name"]')" == "archivos-$(date +%Y%m%d).zip" && "$(mf "$TMP/varios.meta.json" 'm["entryCount"]')" == 5 ]] \
    && ok "meta: archivos-$(date +%Y%m%d).zip con 5 archivos" || fail "meta de varios elementos"
else
  fail "preparar o descifrar varios elementos" "$(tail -n 1 "$LOG")"
fi

# ---------------------------------------------------------------------------
echo "== Casos de error"
if [[ -f "$TMP/grande/package.cpk1" ]]; then
  cp "$TMP/grande/package.cpk1" "$TMP/alterado.cpk1"
  python3 -I -c 'import sys; p=sys.argv[1]; b=bytearray(open(p,"rb").read()); b[2*1024*1024]^=1; open(p,"wb").write(b)' "$TMP/alterado.cpk1"
  if "${VERIFY[@]}" --package "$TMP/alterado.cpk1" --key-file "$TMP/grande.key" --meta "$TMP/grande/meta.b64" --out "$TMP/alterado.claro" >/dev/null 2>&1; then
    fail "un paquete alterado se aceptó"
  else
    [[ -e "$TMP/alterado.claro" ]] && fail "quedó claro parcial de un paquete alterado" || ok "paquete con un bit alterado: rechazado y sin claro parcial"
  fi
fi
expect_error() { # NOMBRE CÓDIGO RUTA
  local before after
  before="$(wc -l <"$LOG")"
  if prepare "error-$1" "$3"; then
    fail "$1: se esperaba error $2"
  else
    after="$(tail -n +$((before + 1)) "$LOG")"
    grep -q "$2" <<<"$after" && ok "$1 -> $2" || fail "$1" "no aparece $2 en la bitácora"
  fi
}
expect_error "enlace simbólico seleccionado" path_not_allowed "$SUB/carpeta/enlace-notas"
expect_error "ruta con .." path_not_allowed "$SUB/../../fuera"
expect_error "archivo inexistente" not_found "$SUB/no-existe.txt"
expect_error "interno de Syncthing" not_found ".stfolder"
if "$BIN" prepare -env-file "$ENV_FILE" --folder "$FOLDER" --path "$SUB/grande.bin" --out "$SMOKE_DIR/salida" >/dev/null 2>>"$LOG"; then
  fail "salida dentro de la carpeta sincronizada aceptada"
else
  ok "salida dentro de la carpeta sincronizada -> rechazada"
fi

# ---------------------------------------------------------------------------
echo "== Privacidad y limpieza"
LEAK=0
while IFS= read -r s; do
  [[ -z "$s" ]] && continue
  if grep -qF -- "$s" "$LOG" "$TMP"/*.verify 2>/dev/null; then fail "la bitácora o la salida de Node contienen un nombre o ruta de prueba"; LEAK=1; break; fi
done < <(printf '%s\n' "$SUB" grande.bin carpeta/ notas.txt foto.jpg doc.md enlace-notas "$NFC" "$NFD" "$FOLDER_PATH")
[[ "$LEAK" == 0 ]] && ok "ningún nombre ni ruta en la bitácora de prepare ni en la salida de Node"
LEAK=0
for k in "$TMP"/*.key; do
  key="$(cat "$k")"
  [[ -z "$key" ]] && continue
  if grep -qF -- "$key" "$LOG" "$TMP"/*.verify "$TMP"/*/manifest.json 2>/dev/null; then LEAK=1; fi
done
[[ "$LEAK" == 0 ]] && ok "ninguna clave en la bitácora, los manifiestos ni la salida de Node" || fail "una clave aparece fuera de su archivo"
left="$(find "$STAGING" -mindepth 1 -maxdepth 1 -newer "$MARKER" 2>/dev/null | wc -l | tr -d ' ')"
[[ "$left" == 0 ]] && ok "el staging quedó sin restos de esta prueba" || fail "quedaron $left entradas en el staging"
[[ "$(stat -f '%Lp' "$STAGING" 2>/dev/null || stat -c '%a' "$STAGING")" == 700 ]] && ok "staging con permisos 0700" || fail "el staging no tiene permisos 0700"

echo
echo "== Resultado: $PASS OK, $FAIL fallos (la carpeta $SUB se borra al salir)"
[[ "$FAIL" == 0 ]]
