# Formato del paquete cifrado: Capataz Package v1 (CPK1)

Estado: borrador 0.1 · 2026-10-19 · lo implementan tres piezas que nunca se ven entre sí:
el cifrador (Go, en Capataz), el descifrador (JavaScript en el navegador, en la página de
descarga de Capataz Nube) y el almacén (PHP/Laravel, que solo mueve bytes y nunca descifra).

Este documento es la fuente de verdad del formato. Si el código y este texto discrepan,
manda este texto y se corrige el código. Los vectores de prueba están en
`docs/vectores-cpk1.json` y los tres lados deben pasarlos.

Las palabras DEBE, NO DEBE y PUEDE se usan en sentido normativo.

---

## 1. Qué problema resuelve

El titular comparte una copia congelada de uno o varios elementos mediante una URL que
vive en un hosting compartido. El hosting guarda los bytes, pero no debe poder leerlos.
Por eso Capataz cifra antes de subir y la clave viaja solo en la URL, dentro del fragmento
(lo que va después de `#`), que el navegador nunca envía al servidor:

```
https://sync.softmx.net/s/<token>#<clave>
                          │         └─ 32 bytes aleatorios, base64url (43 caracteres). No sale del navegador.
                          └─ 32 bytes aleatorios, base64url (43 caracteres). Identifica el enlace.
```

`token` y `clave` son independientes entre sí. El servidor solo conoce
`tokenHash = base64url_sin_relleno(SHA-256(token))`, calculado sobre el **texto de 43
caracteres** del token tal como viaja en la URL (bytes ASCII), no sobre los 32 bytes
decodificados. Vectores en [`vectores-nube.json`](vectores-nube.json).

## 2. Primitivas

| Uso | Algoritmo |
|---|---|
| Cifrado autenticado | AES-256-GCM, nonce de 96 bits, etiqueta de 128 bits |
| Derivación de subclaves | HKDF-SHA-256 (RFC 5869) |
| Hash de integridad | SHA-256 |
| Codificación en texto | base64url sin relleno (RFC 4648 §5) |

Todo existe en Go (`crypto/aes`, `crypto/cipher`, `golang.org/x/crypto/hkdf` o
`crypto/hkdf` desde Go 1.24) y en Web Crypto (`AES-GCM`, `HKDF`) sin librerías externas.

## 3. Claves

- `K` (clave del enlace): 32 bytes de `crypto/rand`. Una por enlace, nunca se reutiliza.
- Subclave de contenido: `HKDF-SHA256(ikm=K, salt=<sal de 16 bytes de la cabecera>, info="capataz-cpk1/content", L=32)`.
- Subclave de metadatos: `HKDF-SHA256(ikm=K, salt=<vacía>, info="capataz-cpk1/meta", L=32)`.
  Con sal vacía, HKDF usa 32 bytes en cero (RFC 5869 §2.2); Go y Web Crypto coinciden.

Las dos subclaves son distintas, así que un mismo `K` nunca cifra con la misma clave dos
tipos de dato.

## 4. El paquete

Un paquete es una **cabecera de 32 bytes** seguida de **uno o más bloques**. Es el cuerpo
exacto que se sube a Capataz Nube y que la página de descarga recibe.

### 4.1 Cabecera (32 bytes, en claro)

| Offset | Largo | Campo | Valor |
|---|---|---|---|
| 0 | 4 | magia | ASCII `CPK1` |
| 4 | 1 | versión | `0x01` |
| 5 | 1 | `chunkSizeLog2` | log2 del tamaño de bloque en claro. Capataz escribe `20` (1 MiB). Un lector DEBE aceptar 10 a 24 y rechazar el resto |
| 6 | 1 | banderas | `0x00`. Un lector DEBE rechazar valores distintos de cero |
| 7 | 2 | reservado | `0x0000`. Un lector DEBE rechazar valores distintos de cero |
| 9 | 16 | sal | aleatoria, para HKDF |
| 25 | 7 | prefijo de nonce | aleatorio |

Total: 4 + 1 + 1 + 1 + 2 + 16 + 7 = 32.

### 4.2 Bloques

El contenido en claro se parte en bloques de `C = 2^chunkSizeLog2` bytes. Todos tienen
exactamente `C` bytes salvo el último, que tiene de 1 a `C`. Si el largo total es múltiplo
de `C`, el último bloque es un bloque lleno (no se añade un bloque vacío al final). La única
excepción es el contenido vacío (0 bytes), que produce un solo bloque vacío.

Cada bloque `i` (empezando en 0) se cifra así:

```
nonce_i = prefijo (7 bytes) ‖ i (uint32, big-endian, 4 bytes) ‖ ultimo (1 byte)
ultimo  = 0x01 si es el último bloque, 0x00 en cualquier otro caso
AAD     = la cabecera completa (32 bytes)
salida_i = AES-256-GCM-Seal(subclaveContenido, nonce_i, bloque_i, AAD)   // texto cifrado ‖ etiqueta de 16 bytes
```

Los bloques cifrados se concatenan sin separadores. Cada uno mide `C + 16` bytes, salvo el
último, que mide `len + 16`.

Esta construcción (nonce con contador y bandera de último bloque, cabecera como AAD) es la
de un cifrado autenticado en flujo. Con ella:

- Un bloque modificado no pasa la verificación.
- Reordenar o duplicar bloques cambia su contador, así que falla.
- **Truncar** el paquete en un límite de bloque falla, porque el nuevo último bloque fue
  cifrado con la bandera `0x00`.
- Cambiar un solo bit de la cabecera falla, porque va como AAD de todos los bloques (y la
  sal, además, cambia la subclave).

### 4.3 Tamaño exacto

```
n = max(1, ceil(largoClaro / C))
largoPaquete = 32 + largoClaro + 16 * n
```

El tamaño se conoce antes de cifrar, y por eso `POST /v1/links` de Capataz Nube lo declara
al abrir la subida. El bloque `i` empieza en `32 + i * (C + 16)`, lo que permite descifrar
con peticiones `Range` si algún día se necesita.

### 4.4 Límites

- Contenido máximo por enlace: **500 MiB = 524 288 000 bytes** de contenido en
  claro. Con `C = 1 MiB` son 500 bloques y el paquete mide como máximo **524 296 032 bytes**.
  Capataz Nube publica este tope en `GET /v1/instance` (`limits.maxEncryptedBytes`).
- El contador de bloque es de 32 bits. No se alcanza con ningún tamaño realista.

## 5. Contenido en claro

El contenido es siempre uno de dos tipos, y la página de descarga lo sabe por los metadatos:

- **`file`**: los bytes del archivo tal cual. Un solo elemento seleccionado.
- **`zip`**: un archivo ZIP completo, construido por Capataz. Varios elementos, o una carpeta.
  El navegador NO necesita entender ZIP: descifra el paquete, obtiene el `.zip` y lo guarda.

Reglas para el ZIP (las cumple Capataz; el navegador no las valida):

- ZIP64 permitido. Nombres en UTF-8 con la bandera de lengua activada. Separador `/`.
- Rutas relativas al elemento seleccionado, sin `..` ni rutas absolutas ni raíz.
- Si dos elementos de primer nivel comparten nombre, el segundo se renombra `nombre (2)`.
- La compresión (deflate o sin compresión) es decisión de Capataz.
- Solo regulares y carpetas. Enlaces simbólicos, sockets, FIFO y similares no se incluyen.

## 6. Metadatos cifrados

Los metadatos permiten que la página muestre el nombre, el tamaño y la lista de contenido
antes de descargar, sin que el servidor los conozca. Es un bloque aparte, que se sube
al abrir la subida y se entrega en `GET /s/{token}/meta`.

### 6.1 Formato

```
metaBlob = nonce (12 bytes aleatorios) ‖ AES-256-GCM-Seal(subclaveMeta, nonce, jsonUtf8, AAD="capataz-cpk1/meta")
```

Se transporta en base64url. Tamaño máximo del `metaBlob` ya decodificado: **192 KiB**
(196 608 bytes). Se usa un nonce aleatorio porque cada `K` cifra exactamente un bloque de
metadatos.

### 6.2 JSON (UTF-8, sin espacios sobrantes)

| Campo | Tipo | Notas |
|---|---|---|
| `v` | entero | `1` |
| `kind` | `"file"` o `"zip"` | |
| `name` | texto | Nombre sugerido para guardar (`informe.pdf`, `fotos.zip`). Máximo 200 caracteres |
| `size` | entero | Largo en claro del contenido |
| `mime` | texto | Solo `file`. Orientativo |
| `createdAt` | RFC 3339 UTC | Momento de la copia congelada |
| `label` | texto | Opcional. Etiqueta puesta por el titular |
| `entryCount` | entero | Solo `zip`. Número total de entradas |
| `entries` | lista de `{path, size}` | Solo `zip`. Hasta 500 entradas. Si hay más, `entriesTruncated: true` |
| `entriesTruncated` | booleano | Solo `zip` |

**La página de descarga trata todo esto como texto no confiable.** Debe escribir
`name`, `label` y `path` con `textContent`, nunca como HTML; debe quitar de `name` los
separadores de ruta y los caracteres de control antes de usarlo como nombre de descarga.

## 7. Algoritmos de referencia

### 7.1 Cifrar (Capataz, Go)

```
K        ← 32 bytes aleatorios
sal      ← 16 bytes aleatorios
prefijo  ← 7 bytes aleatorios
cabecera ← "CPK1" ‖ 01 ‖ 14 ‖ 00 ‖ 0000 ‖ sal ‖ prefijo      // 0x14 = 20
clave    ← HKDF-SHA256(K, sal, "capataz-cpk1/content", 32)
escribir cabecera
para i = 0 .. n-1:
    leer hasta C bytes del contenido (un bloque lleno salvo el último)
    ultimo ← (i == n-1)
    escribir Seal(clave, prefijo ‖ be32(i) ‖ ultimo, bloque, cabecera)
```

Para saber si un bloque es el último sin releer, se lee un byte adelantado o se usa el
largo ya conocido: Capataz conoce el largo en claro antes de cifrar.

### 7.2 Descifrar (navegador, JavaScript)

```
recibir cabecera (32 bytes); validar magia, versión, banderas, reservado y rango de chunkSizeLog2
clave ← HKDF(K, sal, "capataz-cpk1/content")
n ← max(1, ceil((largoPaquete - 32) / (C + 16)))       // largoPaquete viene de la respuesta o de /meta
para cada bloque i en orden:
    nonce ← prefijo ‖ be32(i) ‖ (i == n-1 ? 1 : 0)
    claro_i ← AES-GCM.decrypt(clave, nonce, bloque_i, AAD=cabecera)    // lanza excepción si falla
si algún bloque falla: descartar TODO lo descifrado y mostrar error. No entregar el archivo parcial.
al final, el tamaño en claro DEBE coincidir con `size` de los metadatos
```

Notas para el navegador:

- Safari en iOS descifra en memoria, por eso el tope de 500 MiB. Un `Blob` armado con los
  bloques en claro y descargado con `<a download>` es lo más portable.
- Los bloques se pueden descifrar en cuanto llegan por la red; no hace falta esperar todo
  el paquete antes de empezar.
- Si el fragmento (`#...`) falta o no mide 43 caracteres base64url, mostrar "enlace
  incompleto" sin hacer ninguna petición al servidor.

### 7.3 Almacén (Laravel)

PHP guarda y sirve bytes. **No descifra, no lee la cabecera y no necesita la clave.** Debe
conservar el paquete tal cual se subió y servirlo con `Range`. La verificación de
integridad de la subida se hace con SHA-256 del paquete completo (ver contrato de Nube).

## 8. Qué garantiza y qué no

Garantiza:

- Quien obtiene solo los bytes del hosting (copia de seguridad, administrador del servidor,
  filtración de disco, otro sitio en la misma cuenta) no puede leer el contenido ni los
  nombres.
- Cualquier alteración, reordenamiento o truncado del paquete se detecta al descifrar.
- La clave nunca llega al servidor por la URL.

No garantiza:

- **Un hosting comprometido de forma activa.** Quien controla el servidor sirve el JavaScript
  de la página de descarga y podría alterarlo para capturar la clave. El cifrado de
  extremo a extremo con una página servida por el mismo servidor no resuelve esto. Es una
  limitación aceptada y está en `docs/decisiones.md`.
- **Que el servidor no vea los metadatos de tráfico**: tamaño del paquete, momentos de
  subida y descarga, direcciones IP.
- **Que el destinatario no reenvíe la URL completa.** Si la URL con `#clave` se comparte,
  cualquiera con ella puede descifrar mientras exista el enlace.

## 9. Versionado

- Los primeros 5 bytes (`CPK1` + versión) identifican el formato. Un cambio incompatible
  genera `CPK2`; nunca se reinterpreta `CPK1`.
- Los campos de banderas y reservado permiten extensiones compatibles solo si todos los
  lectores las rechazan hoy: en la versión 1 DEBEN ser cero.
- El contrato de Nube lleva `cipherVersion: "cpk1"`; Nube rechaza versiones que no conoce.

## 10. Vectores de prueba

`docs/vectores-cpk1.json` contiene, con clave, sal y prefijo fijos solo para pruebas:

| Caso | Largo en claro | `chunkSizeLog2` | Bloques | Largo del paquete |
|---|---|---|---|---|
| `vacio` | 0 | 10 | 1 (vacío) | 48 |
| `corto` | 40 | 10 | 1 | 88 |
| `un_bloque_exacto` | 1024 | 10 | 1 | 1072 |
| `dos_bloques_exactos` | 2048 | 10 | 2 | 2112 |
| `tres_bloques_con_resto` | 2500 | 10 | 3 (1024+1024+452) | 2580 |

El contenido en claro de cada caso es el byte `i mod 251` en la posición `i`.
El archivo incluye el paquete completo en hexadecimal y su SHA-256, el contenido en claro
(SHA-256), las subclaves derivadas y un `metaBlob` completo con su nonce fijo.

Los vectores se generaron con Python (`cryptography`) y se verificaron de forma
independiente con Node (Web Crypto): descifrado correcto de los cinco casos, igualdad
byte a byte de los hashes de paquete, descifrado del `metaBlob`, y rechazo correcto de:
un bit cambiado en el cifrado, un bit cambiado en la cabecera, truncado en límite de
bloque e intercambio de bloques (25 comprobaciones, 0 fallos).

### Pruebas exigidas a cada implementación

- **Go (cifrador):** cifrar los cinco casos con la sal y el prefijo de los vectores debe dar
  exactamente `packageHex`. Además, un ciclo cifrar y descifrar con aleatorios, incluido
  un archivo de 3 MiB + 1 byte con `chunkSizeLog2 = 20`.
- **JavaScript (descifrador):** descifrar los cinco `packageHex`, el `metaBlob`, y rechazar
  los casos negativos de arriba. Debe probarse en Chrome, Firefox y Safari (iOS incluido).
- **PHP (almacén):** subir y servir un paquete de los vectores y comprobar que los bytes
  recibidos son idénticos (SHA-256), también con peticiones `Range`.
- **Cruzada:** un paquete cifrado por Go debe descifrarse en el navegador, con un archivo
  real, antes de dar por buena la Fase 2.
