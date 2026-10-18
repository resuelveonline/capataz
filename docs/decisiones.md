# Registro de decisiones

## 2026-10-17
- Capataz es un servicio propio junto a Syncthing; no se modifica ni se hace fork
  de Syncthing (la API no entrega contenido y el host ya tiene los archivos).
- El servicio se llama "Capataz" (antes "portal agent").
- Un solo contrato de API para portal web, Android e iOS.
- Los enlaces temporales son copias congeladas en staging fuera de lo sincronizado.
- Desarrollo inicial en la Mac mini M4; despliegue final en Raspberry Pi
  (SSD, no tarjeta SD).
- Código sincronizado entre equipos con Git (GitHub privado), no con Syncthing.

## Pendientes por decidir
- Lenguaje de Capataz (propuesto: Go).
- Si las apps Flutter reemplazan la idea de embeber Syncthing en el móvil.
- Si se requiere trabajo sin conexión en las apps.
- Forma de acceso externo al host (dominio, túnel o VPN).
- Si las apps podrán subir archivos.