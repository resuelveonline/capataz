# Capataz (servicio)

Servicio en Go que corre junto a Syncthing en el host y expone la API de
Capataz (contrato: [`../api/capataz-api.yaml`](../api/capataz-api.yaml)).
Fase 1: API de **solo lectura**. Fase 2a: orden local `capataz prepare` que
arma y cifra el paquete de un enlace. Fase 2b: enlaces temporales con
`POST /links`, subidos a Capataz Nube (probados contra una nube falsa; el
servidor Laravel aún no existe).

| Ruta | Auth | Qué hace |
|---|---|---|
| `GET /instance` | pública | identidad de la instancia (`instanceId`, `name`, `version`, `publicMode`) |
| `GET /folders` | Bearer | carpetas de Syncthing (`id` y `label`) |
| `GET /files?folder=&path=` | Bearer | lista una carpeta (`Entry`: `name`, `path`, `isDir`, `size`, `modified`) |
| `GET /files/download?folder=&path=` | Bearer | descarga un archivo; admite `Range`, `If-Range` y `HEAD` |
| `POST /links` | titular | crea un enlace temporal; responde 202 en `preparing` |
| `GET /links?status=` | titular | lista los enlaces, del más reciente al más antiguo |
| `GET /links/{id}` | titular | estado del enlace; con `active` trae la `url` (`…/s/<token>#<clave>`) |
| `DELETE /links/{id}` | titular | revoca (en la nube primero; 502 `cloud_unreachable` si no responde) |

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
| `CAPATAZ_STAGING_DIR` | no | copias congeladas temporales, por defecto `~/capataz-host/staging`; debe ser 0700 y estar fuera de toda carpeta sincronizada |
| `CAPATAZ_NUBE_URL` | no | Capataz Nube, por defecto `https://sync.softmx.net`; `http` solo en loopback |
| `CAPATAZ_NUBE_KEY` | no | clave de instancia de la nube, 32 bytes en base64url (secreto). Vacía: los enlaces responden `cloud_unreachable` |

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

## Fase 2a: preparar un paquete cifrado

```sh
capataz/bin/capataz prepare -env-file .env --folder fgyww-edjwa \
    --path subcarpeta --path uno.txt --out /tmp/paquete [--label "Entrega"] [--print-key]
```

1. **Copia congelada** (`internal/stage`): cada ruta se resuelve sin seguir
   enlaces simbólicos, se fuerza `db/scan?sub=`, se espera a que el índice
   alcance al disco y se valida cada archivo contra `db/file` y `db/need`.
   Después se copia de verdad al staging, se re-verifica el origen y se
   reintenta hasta 3 veces si cambió.
2. **Carga útil** (`internal/bundle`): un archivo tal cual, o un ZIP si hay
   varios elementos o una carpeta. Tope: 524 288 000 bytes de carga útil.
3. **Paquete** (`internal/cpk`): CPK1 según
   [`../docs/formato-paquete-cifrado.md`](../docs/formato-paquete-cifrado.md).

En `--out` (0700, fuera de las carpetas sincronizadas, nunca se sobrescribe)
quedan `package.cpk1`, `meta.b64` (metaBlob en base64url) y `manifest.json`
(linkId, tamaños, SHA-256 de carga útil y paquete, omitidos por motivo). El
manifiesto **no** lleva nombres ni la clave. La clave solo sale por stdout,
una vez, con `--print-key` (para pruebas); nunca va a la bitácora.

Descifrar como lo hará el navegador (Web Crypto, sin dependencias):

```sh
capataz/bin/capataz prepare ... --print-key > /tmp/clave   # con umask 077
node scripts/cpk1-verify.mjs --package /tmp/paquete/package.cpk1 --key-file /tmp/clave \
    --meta /tmp/paquete/meta.b64 --out /tmp/claro [--meta-out /tmp/meta.json]
node scripts/cpk1-verify.mjs --vectors docs/vectores-cpk1.json   # vectores y casos negativos
```

La clave se lee de un archivo o de stdin (`-`), nunca de un argumento.

### Prueba de humo de la Fase 2a

```sh
scripts/fase2a-smoke.sh
```

Crea contenido en `pruebas/_smoke-fase2a/` (con un marcador; se borra
siempre al salir), prepara un archivo de 3 MiB + 1 byte, una carpeta, nombres
con ñ en NFC y NFD y varios elementos; descifra cada paquete con
`cpk1-verify.mjs`, compara SHA-256 con el original y prueba los ZIP con
`unzip -t`. Termina comprobando casos de error, que la bitácora no contenga
nombres ni claves y que el staging quede vacío.

## Fase 2b: enlaces temporales

```sh
curl -H @auth-header -H 'Content-Type: application/json' \
  --data '{"items":[{"folder":"fgyww-edjwa","path":"informe.pdf"}],"ttlSeconds":86400}' \
  http://127.0.0.1:8080/links          # 202 {"id":..., "status":"preparing"}
curl -H @auth-header http://127.0.0.1:8080/links/<id>   # hasta status=active
```

`POST /links` valida en el acto la forma, la vigencia (1 h a 30 días), que
las carpetas existan y que las rutas sean admisibles (400, 404). En segundo
plano: copia congelada, ZIP si hace falta, CPK1, apertura de la subida,
partes de 8 MiB (reanudables, con reintentos) y cierre con el SHA-256 del
paquete. Lo que falle ahí llega en `failureCode` con `status=failed`.

Cada enlace se guarda en `CAPATAZ_STATE_DIR/links/<id>.json` (0600, en un
directorio 0700) con su token y su clave K, que se borran al vencer, revocar
o fallar. Tras un reinicio, una subida a medias se reanuda (solo las partes
que faltan) y una preparación a medias queda `failed`. El vencimiento se
aplica en local cada 30 s; cada 5 min se concilian con la nube las descargas
y los estados `exhausted` (se muestra como `expired`), `expired` y
`revoked`.

### Prueba de humo de la Fase 2b

```sh
scripts/fase2b-smoke.sh
```

Arranca `cmd/nubefalsa` (la nube falsa de `internal/nube/nubetest`, en
`127.0.0.1:18090`, con una clave aleatoria en un archivo 0600) y Capataz en
el puerto 18082 con un estado y un staging temporales propios. Crea enlaces
para un archivo de 9 MiB (dos partes) y una carpeta, los descarga por la
cara pública de la nube y los descifra con `cpk1-verify.mjs`; prueba el tope
de descargas, la revocación, los errores síncronos y que la bitácora no
contenga nombres, tokens ni claves.

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
- 405, 412 y 416 salen con `code` `invalid_request` (documentado en el
  contrato v0.2.3).

## Limitaciones de la Fase 2a

- **`capataz prepare` es una orden de desarrollo:** no sube nada ni guarda
  la clave; los enlaces reales van por `POST /links` (Fase 2b). Una
  ejecución usa una sola carpeta de Syncthing (`stage` ya admite varias).
- **Una consulta a `db/file` por archivo:** carpetas con miles de archivos
  tardan en validarse.
- **Congelado archivo por archivo:** cada archivo se verifica al copiarlo,
  pero el conjunto no es una instantánea atómica (un archivo ya copiado puede
  cambiar mientras se copia el siguiente).
- **Enlaces simbólicos nunca se siguen** (a diferencia del listado y la
  descarga de la Fase 1): seleccionado directamente es `path_not_allowed`;
  dentro de una carpeta se omite y se cuenta.
- **Omitidos solo se cuentan** (ignored, hidden, symlink, special): en el
  manifiesto y en `Link.skipped`, sin nombres.
- **Colisiones de mayúsculas** solo se resuelven en el primer nivel del ZIP.
  Dentro de una carpeta de un sistema que las distingue (ext4 en la Pi),
  `A.txt` y `a.txt` chocarían al descomprimir en macOS o Windows.
- **El ZIP no conserva** atributos extendidos, permisos (todo 0644) ni
  bifurcaciones de recursos de macOS.
- **Disco:** hace falta espacio para el archivo más grande + 2 × la carga útil
  + 64 MiB; si no hay, `insufficient_storage`.
- **`cpk.Decrypt` escribe en flujo:** quien lo use directamente debe
  descartar la salida si devuelve error (`DecryptFile` y `cpk1-verify.mjs` ya
  lo hacen).
- **Sin medir en la Raspberry Pi:** la Pi 4 no tiene extensiones
  criptográficas ARMv8, así que AES-GCM irá bastante más lento que en la Mac
  (la Pi 5 sí las tiene). NFC/NFD solo se probó en APFS.

## Limitaciones de la Fase 2b

- **Sin Capataz Nube real:** todo se probó contra `nubetest`, que sigue el
  contrato pero no es el Laravel. Cuando exista, repetir la prueba de humo
  apuntando a él.
- **Descargas aproximadas:** el host se entera de las descargas al
  conciliar (cada 5 min), no en el momento; la bitácora de la nube es la
  fuente exacta.
- **Una preparación a medias no se reanuda:** tras un reinicio queda
  `failed` (`internal_error`) y hay que crear el enlace de nuevo.
- **Dos preparaciones o subidas a la vez** como máximo; la tercera recibe
  429 `rate_limited`.
- **El token de desarrollo es la única cuenta**, con rol titular; `owner` de
  la bitácora es su deviceId fijo hasta el emparejamiento.
- **La clave K vive en el host** mientras el enlace está vigente (para
  poder devolver la `url` tras la preparación asíncrona): protege el estado
  como al propio `.env`.
- **Sin medir en la Raspberry Pi:** subida, cifrado y copia.

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
cmd/capataz/        main: servidor HTTP (sin subcomando) y `capataz prepare`
internal/config/    variables de entorno y lector de .env (solo desarrollo)
internal/state/     instanceId en CAPATAZ_STATE_DIR/instance.json
internal/syncthing/ cliente de config/folders, db/file, db/scan y db/need
internal/safepath/  validación de rutas y acceso al disco con os.Root
internal/auth/      interfaz Authenticator y DevToken
internal/api/       rutas, middleware, errores JSON y handlers
internal/cpk/       formato CPK1: cifrado y descifrado en flujo, metadatos, clave
internal/stage/     copia congelada validada contra Syncthing (stagetest: Syncthing falso)
internal/bundle/    carga útil: archivo o ZIP, tope y JSON de metadatos
internal/prepare/   orquestación de stage → bundle → cpk y salida del paquete
internal/nube/      cliente HMAC de Capataz Nube y subida por partes (nubetest: nube falsa)
internal/links/     enlaces: estado persistente, tareas, vencimiento, conciliación, revocación
cmd/nubefalsa/      nube falsa en un puerto fijo para la prueba de humo (solo desarrollo)
```
