# Informe: Fase 2c, punto de parada tras 10b y deploy-nube.sh

- Fecha: 2026-10-20
- Rama: `fase-2c-nube`, con el árbol limpio. No hubo push ni se crearon etiquetas.
- Estado: están hechos los commits del 1 al 10b y `scripts/deploy-nube.sh`. Falta que Paco despliegue y pruebe en el hosting real; después siguen los commits del 11 al 15.

## 1. Commits (`git log --oneline main..HEAD`)

```
bfa7db6 scripts: deploy-nube.sh (rsync en simulación por defecto)
6c8d8a7 nube: Range entre partes, HEAD y prueba aleatoria de intervalos
68dd749 nube: /s/{token}/meta y /blob con estados y conteo de descargas
32d07a0 nube: DELETE /v1/links/{id} revoca y borra los bytes
91aeb1d nube: complete verifica partes, tamaño y SHA-256 sin ensamblar
611e9cd nube: PUT de partes por flujo, con sustitución y recibo
a9a52c6 nube: abrir, listar y consultar enlaces
ce393f1 nube: GET /v1/instance con límites y uso
114ddf6 nube: autenticación HMAC de la API de instancia
1fe5a77 nube: capataz:instance:add y capataz:instance:disable
c9b5025 nube: tablas y modelos de instancias, enlaces, partes y nonces
57de7d0 nube: esqueleto de Laravel 13 para Capataz Nube
ff46be8 Contrato de Capataz Nube v0.1.1: agotado con margen, estados públicos, Range y HEAD
8839ccf nubetest: usage cuenta también los enlaces en subida (C4)
1d218b6 links: failureCode de la nube no pasa al enum de Capataz (C6)
1f308ad nubetest: Range de un intervalo, margen de agotado, HEAD y failed como 404
```

Los tres primeros commits (los cambios de Go: C1/C2 en nubetest, C6 en `links.reconcile` y C4) van separados y primero en la rama, como se pidió.

## 2. Resultados de las pruebas

| Verificación | Resultado |
|---|---|
| Go: `go test -race ./...`, `go vet`, `gofmt`, compilación arm64 | 14 paquetes OK |
| `scripts/fase2b-smoke.sh` (nube falsa) | 30/30 |
| Laravel con PHPUnit (`composer test`) | 49 pruebas y 2263 aserciones, OK |
| Vectores de firma y tokenHash (`docs/vectores-nube.json`) | Laravel coincide con los 6 vectores (3 de tokenHash y 3 de firma) |
| Vectores de Range (`docs/vectores-range.json`) | Go y Laravel pasan los mismos 31 casos |
| Range aleatoria | Go: 300 intervalos. Laravel: 500 intervalos con partes de 16 bytes y trozos de lectura de 5 bytes. Cada respuesta se compara con su rebanada del paquete original. |
| `redocly lint` en los dos contratos | limpio (`capataz-api.yaml` v0.2.3 y `capataz-nube-api.yaml` v0.1.1) |
| `scripts/vectores-nube-verify.py` | "todo correcto" |

### Prueba HTTP real en local (`php artisan serve` y un cliente Python desechable)

- Un GET firmado con consulta (`/v1/links?state=active&limit=2`) pasa.
- Cada PUT de 8 MiB tarda unos 0,04 s; el cuerpo se lee como flujo con SHA-256 incremental.
- `complete` deja el enlace en `active`.
- El blob completo (16 MiB) sale idéntico al original.
- Un Range que cruza de la parte 1 a la 2 devuelve 206 con los bytes correctos.
- HEAD con Range devuelve el `Content-Length` correcto y el cuerpo vacío.
- Pedir dos intervalos devuelve 416 con `Content-Range: bytes */size`.
- Al terminar se borraron los datos y la base de prueba.

### `deploy-nube.sh` contra un directorio local (`NUBE_DEPLOY_TARGET`)

- Sin `--apply` es una simulación (`--dry-run`) y no copia nada.
- Con `--apply` no viajan `.env`, bases SQLite, `capataz-datos`, cachés de configuración ni pruebas. De `storage` solo se copia la estructura de directorios vacía. `.env.example` sí se copia.
- La copia desplegada (`public_html/` junto a `capataz-nube/`) arranca con `php -S`.

### Smoke contra Laravel

Todavía no existe. Es el commit 14, que va después de las pruebas en el hosting.

## 3. Cambios respecto al plan y notas

1. **`uploading` responde 409 en `/s`, no 404.** El contrato ya decía `link_not_ready`, y se aplicó tu salvedad. Solo `failed` pasó a 404. HEAD devuelve los mismos estados que GET, incluido 410, y no cuenta como descarga.
2. **Orden de verificación de la firma.** El contrato pide checksum → firma → nonce, así que el cuerpo se lee antes de verificar la firma. Para no leer de más, antes se exige `Content-Length` ≤ `max_part_size`; si no se cumple, 413 sin leer nada. El nonce se guarda solo después de verificar la firma, y la comparación usa `hash_equals`. No se usa sodium.
3. **Tamaño exacto de la parte.** El `partSize` esperado de cada parte se compara con el `Content-Length` y con los bytes escritos una vez volcado el temporal. Por eso una parte de tamaño equivocado se lee entera (8 MiB como mucho) antes de devolver 400.
4. **Commit de Go adicional (`8839ccf`).** nubetest cuenta en `usage` los enlaces en subida (C4).
5. **Mockery como dependencia de desarrollo.** La necesita `RefreshDatabase`.
6. **`package_sha256` en la migración inicial.** Se añadió ahí en vez de crear otra migración, porque aún no se había desplegado en ninguna parte.
7. **Commit 2 corregido con `--amend`.** Se había confirmado con pruebas fallando porque `| tail` ocultó el código de salida. Ahora siempre se usa `set -o pipefail`.
8. **`NUBE_DEPLOY_TARGET` en `deploy-nube.sh`.** Es una variable añadida solo para probar el script en local.
9. **Subidas abandonadas (C5).** Se cuentan desde `last_activity_at`, es decir, desde la última parte recibida o la última reapertura.
10. **Configuración.** El tamaño de parte, los límites y los tiempos se leen de `.env` (`config/capataz.php`). `composer.json` fija `config.platform.php = 8.4.26`.

## 4. Pruebas pendientes en el hosting (las hace Paco)

1. Ejecutar `scripts/deploy-nube.sh` (simulación) y revisar el resultado; después, `scripts/deploy-nube.sh --apply`.
2. En el servidor, con `/opt/alt/php84/usr/bin/php`:
   - Crear `.env` a partir de `.env.example`, con `DB_CONNECTION=mysql`, `CAPATAZ_DATA_DIR=~/domains/sync.softmx.net/capataz-datos` y `APP_ENV=production`.
   - `artisan key:generate`
   - `mkdir -m 700 -p …/capataz-datos`
   - `artisan migrate --force`
   - `artisan config:cache`
   - `artisan capataz:instance:add <instanceId>`. La clave se muestra una sola vez y va al `.env` del host.
3. **PUT de 8 MiB con Imunify360.** Si lo bloquea, en lugar del JSON con el recibo llegará HTML con 403 o 406.
4. **`REQUEST_URI` tras la reescritura de LiteSpeed.** Un GET firmado con consulta debe pasar. Si devuelve `invalid_signature`, la ruta llega alterada.
5. **Range.** 206 para un intervalo que cruce partes, 416 para dos intervalos, y HEAD.
6. **Buffering de LiteSpeed.** Medir un blob grande con `curl -o /dev/null -w '%{time_starttransfer} %{time_total}'`. Si los dos tiempos casi coinciden, LiteSpeed está acumulando la respuesta antes de enviarla.
7. **TrustProxies.** Ver qué llega en `REMOTE_ADDR` y en `X-Forwarded-For`, para configurar `NUBE_TRUSTED_PROXIES`.
8. **Extensiones de PHP.** Confirmar que están pdo_mysql, openssl, mbstring, fileinfo, dom y tokenizer.

> Los puntos 3 a 6 necesitan peticiones firmadas, y el repo todavía no tiene una herramienta para hacerlas. Propuesta: `scripts/nube-probe.py`, solo con la biblioteca estándar y la clave leída de un archivo 0600, que haga esas comprobaciones contra el hosting.

## 5. Limitaciones actuales (commits 11 a 15, pendientes)

| # | Pendiente | Efecto hoy |
|---|---|---|
| 11 | `capataz:sweep` en cron cada minuto | Los cambios por tiempo (vencido, margen de agotado, subida abandonada) solo se aplican cuando alguien consulta el enlace. Mientras tanto no se borran los bytes de enlaces que nadie vuelve a consultar, ni los nonces viejos, ni los directorios huérfanos, ni los temporales de subida. |
| 12 | Límite de peticiones (por instancia y en `/s` por IP) y TrustProxies | Todavía no hay respuestas 429. |
| 13 | Pruebas de privacidad de los logs | No hay una prueba automática de que el token, la clave, el cuerpo y el tokenHash no lleguen a los logs ni a los errores. |
| 14 | `fase2b-smoke.sh` contra una Nube externa (C11) y `fase2c-smoke.sh` | No hay smoke de extremo a extremo de Go contra Laravel. |
| 15 | `nube/README.md` (despliegue, cron, APP_KEY/APP_PREVIOUS_KEYS, riesgos), `decisiones.md` y `CLAUDE.md` | La documentación de despliegue está incompleta. |

### Otras limitaciones

- `/s/{token}` muestra por ahora una página provisional, sin el descifrado en el navegador.
- PHP sirve cada byte de las descargas; no hay sendfile ni X-Accel en el hosting compartido.
- El access log del hosting registra las URLs públicas, que contienen el token. La clave no aparece porque va en el fragmento `#`. Esto está fuera del control de la aplicación.
