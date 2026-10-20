#!/usr/bin/env bash
# Despliega Capataz Nube (nube/) al hosting compartido con rsync por SSH.
#
#   scripts/deploy-nube.sh            muestra qué se copiaría (rsync --dry-run)
#   scripts/deploy-nube.sh --apply    copia de verdad
#   scripts/deploy-nube.sh --apply --delete
#                                     además borra en el servidor lo que ya no
#                                     existe en la versión (nunca .env, storage,
#                                     bases SQLite ni capataz-datos)
#
# Estructura en el servidor (relativa al home remoto):
#   $NUBE_REMOTE_BASE/public_html/    contenido de nube/public, con un index.php
#                                     que apunta a ../capataz-nube
#   $NUBE_REMOTE_BASE/capataz-nube/   la aplicación (fuera de lo público)
#   $NUBE_REMOTE_BASE/capataz-datos/  los paquetes (CAPATAZ_DATA_DIR); este
#                                     script nunca lo toca
#
# Solo se despliega lo confirmado en git (git archive del commit actual), con
# las dependencias de producción instaladas en un directorio temporal según
# composer.lock (resuelto para PHP 8.4.26). Nunca viajan .env, storage (salvo
# su estructura de directorios vacía), bases SQLite, capataz-datos, cachés de
# configuración ni pruebas. El script no contiene ni pide credenciales: usa
# el alias de ~/.ssh/config.
#
# Variables:
#   NUBE_SSH_HOST     alias SSH (por defecto hostinger-sync)
#   NUBE_REMOTE_BASE  base remota (por defecto domains/sync.softmx.net)
#   NUBE_DEPLOY_TARGET  solo para probar el script: un directorio local que
#                     sustituye a HOST:BASE (sin SSH)
set -euo pipefail
set +x

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HOST="${NUBE_SSH_HOST:-hostinger-sync}"
BASE="${NUBE_REMOTE_BASE:-domains/sync.softmx.net}"
TARGET="${NUBE_DEPLOY_TARGET:-$HOST:$BASE}"
APPLY=0
DELETE=0
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=1 ;;
    --delete) DELETE=1 ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "Opción desconocida: $arg" >&2; exit 2 ;;
  esac
done
for cmd in git composer rsync ssh php; do
  command -v "$cmd" >/dev/null || { echo "Falta $cmd" >&2; exit 1; }
done
case "$BASE" in
  ""|/*|*..*) echo "NUBE_REMOTE_BASE debe ser una ruta relativa al home remoto, sin '..'" >&2; exit 2 ;;
esac

cd "$ROOT"
if [[ -n "$(git status --porcelain -- nube)" ]]; then
  echo "Hay cambios sin confirmar en nube/: solo se despliega lo confirmado." >&2
  git status --short -- nube >&2
fi
REV="$(git rev-parse --short HEAD)"

umask 022
BUILD="$(mktemp -d "${TMPDIR:-/tmp}/capataz-nube-deploy.XXXXXX")"
trap 'rm -rf "$BUILD"' EXIT

echo "== Armando la versión $REV en un directorio temporal"
git -c tar.umask=022 archive --format=tar HEAD:nube | tar -x -C "$BUILD"
# Ni por error: ningún .env ni base SQLite en lo que se despliega.
find "$BUILD" \( -name '.env' -o -name '.env.*' ! -name '.env.example' -o -name '*.sqlite' -o -name '*.sqlite-*' \) -print -delete
(cd "$BUILD" && composer install --no-dev --no-interaction --no-progress --prefer-dist --optimize-autoloader --quiet)
rm -f "$BUILD/bootstrap/cache/config.php" "$BUILD/bootstrap/cache/routes-v7.php" "$BUILD/bootstrap/cache/events.php"

# index.php para public_html/: la aplicación vive en ../capataz-nube.
cat >"$BUILD/public/index.php" <<'PHP'
<?php

use Illuminate\Foundation\Application;
use Illuminate\Http\Request;

define('LARAVEL_START', microtime(true));

// public_html/ y capataz-nube/ son hermanos (scripts/deploy-nube.sh).
$app_dir = __DIR__.'/../capataz-nube';

if (file_exists($maintenance = $app_dir.'/storage/framework/maintenance.php')) {
    require $maintenance;
}

require $app_dir.'/vendor/autoload.php';

/** @var Application $app */
$app = require_once $app_dir.'/bootstrap/app.php';
$app->usePublicPath(__DIR__);

$app->handleRequest(Request::capture());
PHP
php -l "$BUILD/public/index.php" >/dev/null

# Permisos 755/644 desde el propio armado (umask 022 y tar.umask=022): el
# rsync de macOS (openrsync) no admite --chmod.
RSYNC=(rsync -rlptz --itemize-changes)
[[ "$APPLY" == 1 ]] || RSYNC+=(--dry-run)
if [[ "$DELETE" == 1 ]]; then
  # P = proteger del borrado aunque no estén en la versión.
  RSYNC+=(--delete --filter='P .env' --filter='P /storage/***' --filter='P *.sqlite*' --filter='P /capataz-datos/***')
fi

APP_FILTERS=(
  --exclude='/public/'
  --exclude='/tests/'
  --exclude='/phpunit.xml'
  # rsync aplica la primera regla que coincide: el include va antes.
  --include='/.env.example' --exclude='.env' --exclude='.env.*'
  --exclude='*.sqlite' --exclude='*.sqlite-*'
  --exclude='/capataz-datos/'
  --exclude='/bootstrap/cache/config.php' --exclude='/bootstrap/cache/routes-*.php'
  # De storage solo la estructura de directorios, sin archivos.
  --include='/storage/' --include='/storage/**/' --exclude='/storage/**'
)

if [[ "$APPLY" == 1 ]]; then
  echo "== Copiando a $TARGET (versión $REV)"
else
  echo "== Simulación (--dry-run) contra $TARGET (versión $REV); nada cambia en el servidor"
fi
echo "-- capataz-nube/"
"${RSYNC[@]}" "${APP_FILTERS[@]}" "$BUILD/" "$TARGET/capataz-nube/"
echo "-- public_html/"
"${RSYNC[@]}" --exclude='.env*' "$BUILD/public/" "$TARGET/public_html/"

cat <<EOF

== Siguiente (a mano, en el servidor: ssh $HOST)
  cd $BASE/capataz-nube
  # Solo la primera vez: crear .env desde .env.example (DB_CONNECTION=mysql,
  # CAPATAZ_DATA_DIR=\$HOME/$BASE/capataz-datos, APP_ENV=production), y:
  #   /opt/alt/php84/usr/bin/php artisan key:generate
  #   mkdir -m 700 -p \$HOME/$BASE/capataz-datos
  /opt/alt/php84/usr/bin/php artisan migrate --force
  /opt/alt/php84/usr/bin/php artisan config:cache
  # Alta de la instancia (la clave sale una sola vez; guárdala en el .env del host):
  #   /opt/alt/php84/usr/bin/php artisan capataz:instance:add <instanceId>
  # Cron (cada minuto), cuando exista la tarea programada:
  #   * * * * * /opt/alt/php84/usr/bin/php \$HOME/$BASE/capataz-nube/artisan schedule:run >/dev/null 2>&1
EOF
[[ "$APPLY" == 1 ]] || echo "(Para copiar de verdad: scripts/deploy-nube.sh --apply)"
