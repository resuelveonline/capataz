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
- Lenguaje de Capataz: Go (confirmado), solo librería estándar salvo
  justificación previa.
- Las apps Flutter son clientes de la API, sin embeber Syncthing y sin trabajo
  sin conexión en la v1 (confirmado).
- Capataz solo abre conexiones salientes; nunca exige puertos abiertos.
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
- Cifrado opcional desde el inicio: AES-256-GCM por bloques, clave por enlace
  en el fragmento # de la URL, nunca guardada en el servidor. Tope provisional
  de 500 MB por enlace cifrado.
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
- Variables: SYNCTHING_URL, SYNCTHING_API_KEY, CAPATAZ_DEV_TOKEN (≥32),
  CAPATAZ_STATE_DIR obligatorias; CAPATAZ_LISTEN (127.0.0.1:8080) y
  CAPATAZ_INSTANCE_NAME opcionales. El entorno real gana sobre el .env.
- Nunca imprimir el token ni el API key: usar archivos de headers 0600 con
  `curl -H @archivo`.

## Convenciones
- Código y comentarios en español donde ayude (nombres de dominio:
  expediente, enlace, titular); identificadores técnicos en inglés.
- Una rama por tarea, fusionar a main al terminar.
- Toda decisión importante se anota en docs/decisiones.md.

## Estado actual
- Fase 0: completada (API de Syncthing validada con curl).
- Fase 1 (rama fase-1-servicio, implementada): Capataz en Go, solo lectura (GET /instance,
  /folders, /files, /files/download) con token de desarrollo por .env.
- Después: emparejamiento de dispositivos, enlaces, Capataz Nube.
