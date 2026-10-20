# Registro de decisiones

## 2026-10-17
- Capataz es un servicio propio junto a Syncthing; no se modifica ni se hace fork
  de Syncthing (la API no entrega contenido y el host ya tiene los archivos).
- El servicio se llama "Capataz" (antes "portal agent").
- Un solo contrato de API para portal web, Android e iOS.
- Los enlaces temporales son copias congeladas, fuera de lo sincronizado.
- Código sincronizado entre equipos con Git (GitHub privado), no con Syncthing.

## 2026-10-18 (servidor y entornos)
- La Mac mini M4 es el servidor de sync (host) actual; la Raspberry Pi queda como
  destino futuro (SSD, no tarjeta SD).
- Datos del host en ~/capataz-host, separados de los de desarrollo (~/capataz-dev).
- Sin datos reales hasta tener respaldo (Time Machine) y FileVault activo.

## 2026-10-18 (resultados de la Fase 0, Syncthing v2.1.6)
- El índice de Syncthing puede ir por detrás del disco; Capataz fuerza
  db/scan con sub= y verifica tamaño y fecha antes de copiar.
- Un archivo recién creado devuelve texto plano ("No such object in the index");
  un archivo borrado sigue devolviendo JSON con deleted=true; ambos casos se rechazan.
- Las rutas con subcarpetas funcionan si se codifican en la URL.
- Se verifica el archivo original antes y después de copiar; si cambia, se reintenta.

## 2026-10-18 (conectividad y copia en hosting)
- No habrá dominio propio nuevo. Etapa 1: Capataz y portal en localhost.
- Capataz solo abre conexiones salientes; nunca exige puertos abiertos.
- Las copias de los enlaces viven en el hosting compartido (Capataz Nube), en
  https://sync.softmx.net; los archivos de sync siguen en el host (Mac mini).
- El hosting es la autoridad de vencimiento y solo guarda el hash del token.
  Revocar requiere que Capataz tenga conexión; vencer no.
- Autenticación Capataz -> hosting: clave por instancia + HMAC con marca de
  tiempo y nonce, sobre HTTPS. Subida por partes reanudables.
- POST /links es asíncrono (202). Estados: preparing, uploading, active, failed,
  expired, revoked.
- La instancia tiene un instanceId propio, generado en el primer arranque.

## 2026-10-18 (cifrado y Fase 1)
- Confirmado: Go para Capataz; apps Flutter como clientes de la API, sin trabajo
  sin conexión en la v1; vigencia de enlaces de 1 hora a 30 días (24 h por defecto).
- Cifrado desde el inicio: AES-256-GCM por bloques, clave por enlace en el
  fragmento # de la URL, mostrada una sola vez y nunca guardada en el servidor.
  Con varios elementos se cifra el ZIP completo.
- Tope provisional por enlace en modo cifrado: 500 MB (ajustar con pruebas, sobre
  todo Safari/iOS).
- Limitación conocida: el cifrado no protege ante compromiso activo del hosting
  (JavaScript alterado de la página de descarga).
- Capataz Nube en Laravel (PHP 8.4 y Composer disponibles por SSH). Estructura en
  el hosting: public_html (público), capataz-nube (Laravel) y capataz-datos
  (paquetes cifrados), los dos últimos fuera de public_html.
- El hosting es una cuenta compartida con otros sitios propios. Riesgo aceptado
  solo para desarrollo; revisar aislamiento (cuenta separada) antes de usar datos reales.
- Fase 1: Capataz en Go, solo lectura (instance, folders, files, download), con
  token de desarrollo por .env. Emparejamiento y enlaces vienen después.

## 2026-10-19 (Fase 1 implementada)
- Contrato v0.2.2: 401 en operaciones protegidas, 500 en todas, 502 en las que
  hablan con Syncthing o Capataz Nube, 416 en las descargas; códigos nuevos
  syncthing_unavailable (502) e internal_error (500).
- Rutas: validación de texto (sin "..", absolutas, bytes nulos ni "\") y
  acceso al disco con os.Root, que impide salir de la carpeta también por
  enlaces simbólicos y sin carreras entre validar y abrir. Los ocultos de
  Syncthing se comparan sin distinguir mayúsculas (APFS) y responden 404.
- Sin WriteTimeout global (cortaría descargas grandes): 30 s por petición en
  respuestas normales y, en descargas, plazo de 2 min por inactividad
  renovado con http.ResponseController. ReadHeaderTimeout e IdleTimeout en
  el servidor.
- Range no satisfacible: 416 con code invalid_request, cuerpo JSON del
  contrato y Content-Range: bytes */<tamaño>. No se añade un código propio.
- Bitácora: solo método, patrón de ruta, estado, bytes y duración. Nunca URL
  real, cadena de consulta, headers, rutas ni nombres de archivo (pueden ser
  datos de expedientes). Lo verifican una prueba unitaria y el smoke.
- Identidad de la instancia: instanceId aleatorio (128 bits) en
  CAPATAZ_STATE_DIR/instance.json, en el host ~/capataz-host/state (no en
  ~/capataz-dev). Se respalda; si está dañado, Capataz no arranca.
- db/file de Syncthing v2.1.6 responde 404 text/plain "No such object in the
  index" tanto para archivos fuera del índice como para carpetas
  inexistentes: Capataz valida la carpeta contra config/folders primero.
- Autenticación de desarrollo: un token por .env (≥32 caracteres), guardado
  como SHA-256 y comparado en tiempo constante; interfaz Authenticator para
  el emparejamiento real.

## 2026-10-19 (cifrado y contrato de Capataz Nube)
- Formato del paquete: CPK1 (docs/formato-paquete-cifrado.md, vectores en
  docs/vectores-cpk1.json). AES-256-GCM con nonce de contador y bandera de
  último bloque, subclaves HKDF-SHA-256, cabecera como AAD; detecta
  alteración, reordenamiento y truncado. Un cambio incompatible será CPK2.
- Bloques de 1 MiB en claro (chunkSizeLog2 = 20).
- Subida a Capataz Nube en partes de 8 MiB (la nube acepta de 1 a 8 MiB),
  reanudable e independiente de los límites de bloque.
- Tope por enlace decidido: 500 MiB de contenido en claro (524 288 000
  bytes), es decir 524 296 032 bytes de paquete. Sustituye al tope
  provisional de 500 MB del 2026-10-18. La nube lo publica en
  limits.maxEncryptedBytes.
- Token y clave los genera Capataz: 32 bytes aleatorios cada uno, en
  base64url e independientes. La clave va solo en el fragmento # de la URL;
  la nube guarda únicamente SHA-256(token).
- El vencimiento lo fija Capataz al abrir la subida (hora de la solicitud
  más ttlSeconds) y la nube lo hace cumplir aunque Capataz esté apagado.
- El nombre del archivo viaja cifrado en los metadatos (metaBlob); la
  descarga de /s/{token}/blob no lleva Content-Disposition.

## 2026-10-19 (Fase 2a: copia congelada, ZIP y CPK1)
- Cifrado CPK1 con la biblioteca estándar (crypto/hkdf desde Go 1.24): sin
  x/crypto. Los cinco vectores se reproducen exactamente en Go y se
  descifran en Node (Web Crypto) con scripts/cpk1-verify.mjs.
- La 2a NO sigue enlaces simbólicos, aunque la Fase 1 sí los siga al listar
  y descargar (si apuntan dentro de la carpeta). Motivo: Syncthing
  sincroniza el enlace como enlace (tipo SYMLINK en su índice), no el archivo
  al que apunta, así que el destino no se puede validar contra db/file ni
  db/need; además, una copia congelada debe contener exactamente lo que
  Syncthing tiene por sincronizado. Seleccionado directamente responde
  path_not_allowed; dentro de una carpeta se omite y se cuenta.
- Raíz del ZIP con una sola carpeta: fotos/x.jpg (la carpeta es el primer
  nivel). Con varios elementos, cada uno en el primer nivel.
- entries y entryCount cuentan solo archivos; las carpetas vacías sí van
  dentro del ZIP.
- Ignorados por .stignore (y ocultos, enlaces y especiales) dentro de una
  carpeta se omiten; el manifiesto los cuenta por motivo (ignored, hidden,
  symlink, special), sin nombres. En la fase del enlace se mostrarán como
  aviso.
- Nombre del ZIP: una carpeta, "<carpeta>.zip"; varios elementos,
  "<label>.zip" o "archivos-AAAAMMDD.zip" con la fecha local del host.
  Colisiones de primer nivel sin distinguir mayúsculas: "informe (2).pdf".
- Store para extensiones ya comprimidas (zip, jpg, png, mp4, pdf, docx...) y
  Deflate para el resto.
- Tope medido sobre la carga útil (el archivo o el ZIP): rechazo antes de
  copiar si un archivo suelto o la cota inferior del ZIP ya lo superan, y
  corte en cuanto el ZIP lo supera mientras se escribe.
- Disco: las copias se hacen de una en una y se borran al añadirlas al ZIP;
  la carga útil se borra en cuanto se cifra. Pico: archivo más grande + ZIP
  + paquete. Se exige ese espacio + 64 MiB antes de empezar.
- Staging: 0700 (si existe con permisos más abiertos, Capataz se niega a
  usarlo en lugar de cambiarlos), fuera de toda carpeta sincronizada (igual
  que el directorio de salida), una preparación por directorio prep-<id> que
  se borra al terminar o fallar; huérfanos de más de 24 h se limpian al
  arrancar.
- Nombres NFC y NFD con ñ funcionan contra Syncthing v2.1.6 en APFS: no se
  añade golang.org/x/text.
- La clave K solo sale por stdout con --print-key; el manifiesto lleva
  linkId, tamaños y SHA-256 de carga útil y paquete, nunca nombres ni K.
  cpk1-verify.mjs lee la clave de un archivo o de stdin, no de argumentos.

## 2026-10-19 (etapa de desarrollo: la Mac emula la Pi)
- Etapa de desarrollo: la Mac mini M4 es el host y emula la Raspberry Pi. Las
  pruebas de funcionalidad valen para la Pi; las de rendimiento (descargas por
  sendfile, cifrado, compresión del ZIP, copia a staging) NO se extrapolan y
  se miden en la Pi cuando exista. Mantener el binario compilable para arm64
  (make build-arm64) y el uso de memoria constante.

## Pendientes por decidir
- Aislamiento del hosting antes de datos reales.
- Límites de PHP del subdominio (hPanel) y estado del SSL de sync.softmx.net.
- Cómo llegan las apps al host fuera de casa (propuesto: Tailscale en desarrollo).
- Si las apps podrán subir archivos (propuesto: Fase 3).
- Contrato v0.2.3 de Capataz: 304, 405 y 412 documentados (hoy 405 y 412 salen
  como invalid_request; 304 lo genera ServeContent con If-Modified-Since);
  encryption=client por defecto en LinkRequest; el estado exhausted de la
  nube se muestra como expired en Link.status; Link con payloadSha256,
  payloadSize, packageSha256 y packageSize (en lugar de sha256 y sizeBytes,
  ambiguos); código insufficient_storage para disco lleno en el staging.
- Medir el rendimiento en la Raspberry Pi, anotando el modelo de Pi y el tipo
  de disco (SSD por USB, tarjeta SD...) de cada medición:
  - descargas: el ResponseWriter envuelto (bitácora y plazo por inactividad)
    desactiva sendfile;
  - cifrado CPK1 (AES-256-GCM): la Pi 4 no tiene extensiones criptográficas
    ARMv8 y la Pi 5 sí;
  - deflate del ZIP;
  - copia a staging.
