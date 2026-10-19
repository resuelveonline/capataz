# Capataz (servicio)

Servicio en Go que corre junto a Syncthing en el host y expone la API de
Capataz (contrato: [`../api/capataz-api.yaml`](../api/capataz-api.yaml)).
Fase 1: **solo lectura**.

| Ruta | Auth | Qué hace |
|---|---|---|
| `GET /instance` | pública | identidad de la instancia (`instanceId`, `name`, `version`, `publicMode`) |
| `GET /folders` | Bearer | carpetas de Syncthing (`id` y `label`) |
| `GET /files?folder=&path=` | Bearer | lista una carpeta (`Entry`: `name`, `path`, `isDir`, `size`, `modified`) |
| `GET /files/download?folder=&path=` | Bearer | descarga un archivo; admite `Range`, `If-Range` y `HEAD` |

`folder` es el **id** de Syncthing (p. ej. `fgyww-edjwa`), no el label.
`path` es relativa a la carpeta, con `/` como separador; vacía es la raíz.
Todos los errores son JSON `{code, message}` con los códigos del contrato.

## Requisitos

- Go 1.27 (solo librería estándar, sin dependencias externas).
- Syncthing en marcha con la REST API accesible (en desarrollo,
  `http://127.0.0.1:8384`).
- `curl` y `python3` para el script de humo.

## Configuración

Variables de entorno. En desarrollo se cargan desde `../.env` (ver
[`../.env.example`](../.env.example)); en producción las entrega systemd con
`EnvironmentFile=` y el binario no lee ningún `.env`. El entorno real
siempre tiene prioridad sobre el `.env`.

| Variable | Obligatoria | Descripción |
|---|---|---|
| `SYNCTHING_URL` | sí | URL de la REST API de Syncthing |
| `SYNCTHING_API_KEY` | sí | API key de Syncthing (secreto; nunca sale en respuestas ni bitácora) |
| `CAPATAZ_DEV_TOKEN` | sí | token temporal de desarrollo, mínimo 32 caracteres (`openssl rand -base64 32`) |
| `CAPATAZ_STATE_DIR` | sí | estado de la instancia, p. ej. `~/capataz-host/state` |
| `CAPATAZ_LISTEN` | no | dirección de escucha, por defecto `127.0.0.1:8080` |
| `CAPATAZ_INSTANCE_NAME` | no | nombre público en `GET /instance`, por defecto `Capataz` |

Si falta alguna obligatoria, el servicio no arranca y el mensaje nombra la
variable, nunca su valor.

## Compilar, ejecutar y probar

Desde `capataz/`:

```sh
make run          # ejecuta con ../.env (go run ... -env-file ../.env)
make test         # go test -race ./...
make vet          # go vet ./...
make build        # bin/capataz para esta máquina
make build-arm64  # bin/capataz-linux-arm64 para la Raspberry Pi (GOOS=linux GOARCH=arm64)
```

Probar a mano (sin escribir el token en la terminal ni en el historial):

```sh
curl -s http://127.0.0.1:8080/instance
printf 'Authorization: Bearer %s\n' "$(grep '^CAPATAZ_DEV_TOKEN=' ../.env | cut -d= -f2-)" > /tmp/capataz-auth && chmod 600 /tmp/capataz-auth
curl -s -H @/tmp/capataz-auth http://127.0.0.1:8080/folders
curl -s -H @/tmp/capataz-auth -G http://127.0.0.1:8080/files --data-urlencode folder=fgyww-edjwa --data-urlencode path=subcarpeta
rm /tmp/capataz-auth
```

### Prueba de humo

Desde la raíz del repositorio:

```sh
scripts/fase1-smoke.sh
```

Compila Capataz, lo arranca en `127.0.0.1:18081` (para no chocar con un
`make run`), recorre todos los endpoints contra la carpeta `pruebas` y al
final comprueba que la bitácora del servidor no contenga nombres de archivo
de la carpeta ni el token. Sale con código distinto de 0 si algo falla. El
token nunca se imprime.

Variables opcionales: `SMOKE_FOLDER` (id, por defecto `fgyww-edjwa`),
`SMOKE_LABEL` (`pruebas`), `SMOKE_FOLDER_PATH`
(`~/capataz-dev/sync/pruebas`) y `SMOKE_PORT` (`18081`). La carpeta necesita
en la raíz al menos un archivo de 4 bytes o más.

## Probar desde otro equipo de la red

Por defecto Capataz solo escucha en `127.0.0.1`. Para probarlo desde otro
equipo o un teléfono de la misma red, cambia `CAPATAZ_LISTEN`, sin editar el
`.env`:

```sh
CAPATAZ_LISTEN=0.0.0.0:8080 make run           # todas las interfaces
CAPATAZ_LISTEN=192.168.1.20:8080 make run      # solo la IP de la Mac en la LAN
```

macOS pedirá permiso en el firewall la primera vez.

> **Aviso:** en la Fase 1 Capataz habla **HTTP sin TLS**. El token y los
> archivos viajan en claro por la red. Úsalo fuera de `127.0.0.1` solo en
> redes de confianza (tu red de casa, o Tailscale) y nunca expuesto a
> Internet. Al arrancar fuera de localhost, el servicio lo avisa en la
> bitácora.

## Identidad de la instancia: respáldala

En el primer arranque Capataz genera un `instanceId` aleatorio y lo guarda en
`$CAPATAZ_STATE_DIR/instance.json` (en el host: `~/capataz-host/state/instance.json`,
directorio 0700 y archivo 0600). **Es la identidad de la instancia**: Capataz
Nube y los dispositivos emparejados la usarán para reconocerla. Si se pierde,
la instancia pasa a ser otra.

- Inclúyela en el respaldo (Time Machine en la Mac mini; copia aparte en la Pi).
- No la pongas en `~/capataz-dev` (datos desechables) ni en el repositorio
  (`.gitignore` la excluye por si acaso).
- Si el archivo está dañado, Capataz no arranca en lugar de generar una
  identidad nueva en silencio.

## Limitaciones de la Fase 1

- **Sin paginación:** `GET /files` devuelve la carpeta completa en una sola
  respuesta. Una carpeta con decenas de miles de elementos será lenta.
- **Sin permisos por cuenta:** se exponen todas las carpetas que reporta
  Syncthing a cualquiera que tenga el token.
- **HTTP sin TLS:** solo para redes de confianza (ver arriba).
- **Un único token de desarrollo**, con rol titular. Todavía no hay
  emparejamiento de dispositivos ni revocación; `auth.Authenticator` es la
  costura para añadirlos.
- **El listado y la descarga leen el disco**, no el índice de Syncthing (que
  puede ir por detrás). La validación con `db/file` y `db/need` llegará con
  los enlaces.
- **Enlaces simbólicos:** se siguen si apuntan dentro de la carpeta; los que
  salen de ella no aparecen en el listado y su descarga responde
  `path_not_allowed`.
- **Rendimiento de descarga:** el `ResponseWriter` envuelto (bitácora y plazo
  por inactividad) desactiva `sendfile`. Pendiente medirlo en la Raspberry Pi.
- 405 y 412 salen como `invalid_request` y todavía no están documentados en
  el contrato (pendiente v0.2.3).

## Decisiones de diseño (resumen)

Detalle en [`../docs/decisiones.md`](../docs/decisiones.md).

- **Rutas seguras con `os.Root`:** el sistema operativo impide salir de la
  carpeta, también por enlaces simbólicos, sin carreras entre validar y
  abrir. Antes se valida el texto (`..`, rutas absolutas, bytes nulos, `\`)
  para responder `path_not_allowed`.
- **Ocultos de Syncthing** (`.stfolder`, `.stversions`, `.stignore`,
  `.syncthing.*.tmp`, `~syncthing~*.tmp`) se comparan sin distinguir
  mayúsculas, porque APFS no las distingue. Pedirlos devuelve 404.
- **Sin `WriteTimeout` global:** cortaría descargas grandes. Las respuestas
  normales tienen 30 s por petición y las descargas un plazo de 2 min **sin
  avanzar**, renovado con `http.ResponseController`.
- **416** sale como JSON `invalid_request` y conserva
  `Content-Range: bytes */<tamaño>`.
- **Bitácora:** método, patrón de ruta (`GET /files/download`), estado, bytes
  y duración. Nunca la URL real, la consulta, headers, rutas ni nombres de
  archivo.
- **Token de desarrollo:** se guarda solo su SHA-256 y se compara en tiempo
  constante.

## Estructura

```
cmd/capataz/        main: configuración, estado, servidor HTTP y apagado ordenado
internal/config/    variables de entorno y lector de .env (solo desarrollo)
internal/state/     instanceId en CAPATAZ_STATE_DIR/instance.json
internal/syncthing/ cliente de config/folders, db/file, db/scan y db/need
internal/safepath/  validación de rutas y acceso al disco con os.Root
internal/auth/      interfaz Authenticator y DevToken
internal/api/       rutas, middleware, errores JSON y handlers
```
