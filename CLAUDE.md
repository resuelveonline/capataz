# Capataz

Servicio propio que corre junto a Syncthing en el host (Raspberry Pi) y expone
una API para el portal web y las apps Flutter (Android/iOS). Permite navegar,
subir y descargar archivos, y generar enlaces temporales de solo lectura.

## Arquitectura
- Syncthing (sin modificar) mantiene la sincronización. Capataz habla con su
  REST API local y lee el disco del host. Syncthing NO sirve contenido de archivos.
- Clientes: portal web, app Flutter Android, app Flutter iOS. Todos consumen la
  misma API de Capataz (contrato en api/capataz-api.yaml).
- Las apps y el portal NUNCA hablan directo con la API de Syncthing; su API key
  vive solo en el host.
- Lenguaje de Capataz: Go (propuesto) [POR CONFIRMAR]
- Las apps Flutter son clientes de la API (sin embeber Syncthing) en la v1
  [POR CONFIRMAR]

## Enlaces temporales (regla central)
- Solo la cuenta titular de la instancia puede crearlos.
- Al crear el enlace se hace una COPIA real a una carpeta de staging fuera de
  cualquier carpeta sincronizada; no sigue cambios posteriores.
- Varios archivos se entregan como ZIP.
- Token aleatorio largo, vigencia configurable, descarga de solo lectura.
- Al expirar o revocar: se borra la copia y se invalida el token.
- Se valida con Syncthing (db/file, db/need) que el archivo exista y no esté
  a medio sincronizar. Prohibido aceptar rutas con ../ o fuera de las carpetas
  que reporta Syncthing.

## Seguridad
- Secretos solo en .env (nunca en el repo). Ver .env.example.
- HTTPS obligatorio en producción. Tokens por dispositivo, revocables.
- Bitácora de quién generó qué enlace y cuándo se descargó.
- Los permisos se verifican en el servidor, no en la app.

## Entornos
- Desarrollo: Mac mini M4 / MacBook, Syncthing con carpeta de pruebas en
  ~/capataz-dev/sync y staging en ~/capataz-dev/staging (fuera del repo).
- Producción: Raspberry Pi, Raspberry Pi OS Lite 64 bits, SSD por USB,
  servicios systemd para Syncthing y Capataz.
- Compilación cruzada a la Pi: GOOS=linux GOARCH=arm64.

## Convenciones
- Código y comentarios en español donde ayude (nombres de dominio:
  expediente, enlace, titular); identificadores técnicos en inglés.
- Una rama por tarea, fusionar a main al terminar.
- Toda decisión importante se anota en docs/decisiones.md.

## Estado actual
- Fase 0: validar la API de Syncthing con curl en la Mac mini.
- Siguiente: contrato OpenAPI y MVP de Capataz.