# Capataz

Servicio propio que corre junto a Syncthing en el host y expone una API para el
portal web y las apps Flutter (Android/iOS). Permite navegar, subir y descargar
archivos, y generar enlaces temporales de solo lectura.

## Arquitectura
- Syncthing (sin modificar, sin fork) mantiene la sincronización. Capataz habla
  con su REST API local y lee el disco del host. Syncthing NO sirve contenido de
  archivos.
- Clientes: portal web, app Flutter Android, app Flutter iOS. Todos consumen la
  misma API de Capataz (contrato en api/capataz-api.yaml).
- Las apps y el portal NUNCA hablan directo con la API de Syncthing; su API key
  vive solo en el host.
- Arquitectura de acceso: un solo portal, el del host de Syncthing. Las apps
  móviles (Android e iOS), el portal web y cualquier computadora acceden
  llamando a la API de Capataz del host; no hay un portal por dispositivo.
  Syncthing es responsable de la sincronización y de las funciones core;
  Capataz es la capa de presentación, permisos y enlaces temporales. Una
  computadora que quiera copia local ejecuta su propio Syncthing, independiente
  de Capataz. Los enlaces temporales se piden siempre al host.
- Lenguaje de Capataz: Go (confirmado), solo librería estándar salvo
  justificación previa.
- Las apps Flutter son clientes de la API, sin embeber Syncthing y sin trabajo
  sin conexión en la v1 (confirmado).
- Capataz no acepta conexiones iniciadas desde Internet ni desde Capataz
  Nube: es el host quien se conecta a la nube para subir, nunca al revés.
  Tampoco se abren puertos en el router. Los clientes (apps, portal web,
  computadoras) llegan al host por la red local o por un canal privado. Cuál
  será ese canal fuera de casa sigue pendiente (propuesto: Tailscale;
  alternativa: un túnel que abra el propio host hacia afuera, p. ej.
  Cloudflare Tunnel).
- Capataz Nube (Laravel, hosting compartido en https://sync.softmx.net) sirve
  las descargas públicas de los enlaces y es la autoridad de vencimiento.
- La instancia tiene un instanceId propio, generado en el primer arranque y
  guardado en CAPATAZ_STATE_DIR/instance.json.

## Enlaces temporales (regla central)
- Solo la cuenta titular de la instancia puede crearlos.
- Al crear el enlace se hace una COPIA congelada fuera de cualquier carpeta
  sincronizada; no sigue cambios posteriores. La copia se sube a Capataz Nube.
- Varios archivos se entregan como ZIP.
- Token aleatorio largo; el hosting solo guarda su hash. Vigencia de 1 hora a
  30 días (24 h por defecto), descarga de solo lectura.
- POST /links es asíncrono (202). Estados: preparing, uploading, active,
  failed, expired, revoked.
- Al expirar o revocar: se borra la copia y se invalida el token. Revocar
  requiere conexión con Capataz Nube; vencer no.
- Cifrado: CPK1, cifrado de extremo a extremo (formato en
  [docs/formato-paquete-cifrado.md](docs/formato-paquete-cifrado.md)).
  Obligatorio desde el contrato v0.2.3: encryption=client por defecto y none
  se rechaza. Clave por enlace en el fragmento # de la URL, nunca en Capataz
  Nube; el host la guarda (0600, en el estado) solo mientras el enlace vive.
- Tope por enlace: 500 MiB de contenido (524 288 000 bytes; 524 296 032 de
  paquete).
- Antes de copiar: db/scan con sub=, validar con db/file y db/need que el
  archivo exista, esté indexado (no texto "No such object in the index"), no
  esté borrado (deleted=true) ni a medio sincronizar. Verificar tamaño y fecha
  antes y después de copiar; si cambia, reintentar.
- Prohibido aceptar rutas con ../ o fuera de las carpetas que reporta Syncthing.

## Seguridad
- Secretos solo en .env (nunca en el repo). Ver .env.example.
- HTTPS obligatorio en producción. Tokens por dispositivo, revocables.
- Bitácora de quién generó qué enlace y cuándo se descargó.
- Los permisos se verifican en el servidor, no en la app.
- Los logs nunca incluyen tokens, API keys, cadenas de consulta ni rutas o
  nombres de archivo (pueden ser datos de expedientes).
- Sin datos reales hasta tener respaldo (Time Machine) y FileVault activo.

## Entornos
- Host actual: Mac mini M4. Datos del host en ~/capataz-host (incluye el estado
  de la instancia en ~/capataz-host/state).
- Desarrollo: Mac mini M4 / MacBook, Syncthing v2.1.6 en http://127.0.0.1:8384,
  carpeta de pruebas "pruebas" (id fgyww-edjwa) en ~/capataz-dev/sync.
  Datos desechables en ~/capataz-dev (fuera del repo).
- Destino futuro: Raspberry Pi, Raspberry Pi OS Lite 64 bits, SSD por USB (no
  tarjeta SD), servicios systemd para Syncthing y Capataz.
- Compilación cruzada a la Pi: GOOS=linux GOARCH=arm64.
- Etapa de desarrollo: la Mac mini M4 es el host y emula la Raspberry Pi. Las
  pruebas de funcionalidad valen para la Pi; las de rendimiento (descargas por
  sendfile, cifrado, compresión del ZIP, copia a staging) NO se extrapolan y
  se miden en la Pi cuando exista. Mantener el binario compilable para arm64
  (make build-arm64) y el uso de memoria constante.

## Compilar, ejecutar y probar (capataz/)
- Requisitos: Go 1.27, solo librería estándar. Módulo
  github.com/resuelveonline/capataz/capataz. Detalle en capataz/README.md.
- `make run`: ejecuta con ../.env (opción -env-file; solo desarrollo).
- `make test`: go test -race ./... (correr también `make vet` y gofmt antes
  de cada commit).
- `make build` → bin/capataz; `make build-arm64` → bin/capataz-linux-arm64
  (GOOS=linux GOARCH=arm64, sin cgo) para la Pi.
- `scripts/fase1-smoke.sh` (desde la raíz): prueba de humo contra la carpeta
  "pruebas" en el puerto 18081, incluida la comprobación de que la bitácora
  no contiene nombres de archivo ni el token.
- `capataz prepare --folder ID --path RUTA ... --out DIR [--print-key]`
  (Fase 2a): copia congelada, ZIP y paquete CPK1 en local.
  `node scripts/cpk1-verify.mjs` lo descifra con Web Crypto (y `--vectors`
  valida docs/vectores-cpk1.json). `scripts/fase2a-smoke.sh` lo prueba todo
  contra "pruebas" en `pruebas/_smoke-fase2a/` (se borra al salir).
- `scripts/fase2b-smoke.sh` (Fase 2b): POST /links de extremo a extremo
  contra "pruebas" y la nube falsa `cmd/nubefalsa` (puertos 18082 y 18090),
  con estado y staging temporales propios; descifra con cpk1-verify.mjs.
- Variables: SYNCTHING_URL, SYNCTHING_API_KEY, CAPATAZ_DEV_TOKEN (≥32),
  CAPATAZ_STATE_DIR obligatorias; CAPATAZ_LISTEN (127.0.0.1:8080),
  CAPATAZ_INSTANCE_NAME, CAPATAZ_STAGING_DIR (~/capataz-host/staging, 0700),
  CAPATAZ_NUBE_URL (https://sync.softmx.net) y CAPATAZ_NUBE_KEY (32 bytes
  base64url; vacía = enlaces desactivados) opcionales. El entorno real gana sobre el .env.
- Nunca imprimir el token, el API key ni la clave K: usar archivos 0600
  (`curl -H @archivo`, `--key-file`). Nombres, rutas y contenido tampoco van a
  bitácora, errores ni mensajes de commit.

## Convenciones
- Código y comentarios en español donde ayude (nombres de dominio:
  expediente, enlace, titular); identificadores técnicos en inglés.
- Una rama por tarea, fusionar a main al terminar.
- Toda decisión importante se anota en docs/decisiones.md.

## Estado actual
- Fase 0: completada (API de Syncthing validada con curl).
- Fase 1 (rama fase-1-servicio, implementada): Capataz en Go, solo lectura (GET /instance,
  /folders, /files, /files/download) con token de desarrollo por .env.
- Fase 2a (rama fase-2a-paquete, implementada): copia congelada, ZIP y
  cifrado CPK1 en local con `capataz prepare`; sin red ni Capataz Nube.
- Fase 2b (rama fase-2b-enlaces, implementada): contrato v0.2.3, cliente
  de Capataz Nube (HMAC, subida por partes reanudable) y POST/GET/DELETE
  /links con estado persistente, probados contra una Capataz Nube falsa
  (internal/nube/nubetest); el servidor Laravel va en una fase aparte.
- Después: Capataz Nube en Laravel, emparejamiento de dispositivos.
