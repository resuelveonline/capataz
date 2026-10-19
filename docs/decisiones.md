cd ~/dev/capataz
cat > docs/decisiones.md <<'EOF'
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

## Pendientes por decidir
- Aislamiento del hosting antes de datos reales.
- Confirmar el tope de 500 MB por enlace.
- Límites de PHP del subdominio (hPanel) y estado del SSL de sync.softmx.net.
- Cómo llegan las apps al host fuera de casa (propuesto: Tailscale en desarrollo).
- Si las apps podrán subir archivos (propuesto: Fase 3).
EOF