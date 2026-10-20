<?php

// Configuración de Capataz Nube (contrato api/capataz-nube-api.yaml v0.1.1).
// Todo tamaño, límite y plazo sale del .env; aquí solo están los valores por
// defecto del contrato.

return [

    // Bytes de los paquetes, fuera de public/. En el hosting:
    // ~/domains/sync.softmx.net/capataz-datos
    'data_dir' => env('CAPATAZ_DATA_DIR', storage_path('app/capataz-datos')),

    'limits' => [
        'max_encrypted_bytes' => (int) env('NUBE_MAX_ENCRYPTED_BYTES', 524296032),
        'min_part_size' => (int) env('NUBE_MIN_PART_SIZE', 1048576),
        'max_part_size' => (int) env('NUBE_MAX_PART_SIZE', 8388608),
        'min_ttl_seconds' => (int) env('NUBE_MIN_TTL_SECONDS', 3600),
        'max_ttl_seconds' => (int) env('NUBE_MAX_TTL_SECONDS', 2592000),
        'max_active_links' => (int) env('NUBE_MAX_ACTIVE_LINKS', 100),
        'max_stored_bytes' => (int) env('NUBE_MAX_STORED_BYTES', 5368709120),
        'max_meta_blob_bytes' => (int) env('NUBE_MAX_META_BLOB_BYTES', 196608),
    ],

    // Segundos.
    'clock_skew' => (int) env('NUBE_CLOCK_SKEW', 300),
    'nonce_ttl' => (int) env('NUBE_NONCE_TTL', 600),
    'ttl_tolerance' => (int) env('NUBE_TTL_TOLERANCE', 300),
    'exhausted_grace' => (int) env('NUBE_EXHAUSTED_GRACE', 3600),
    'upload_abandon_seconds' => (int) env('NUBE_UPLOAD_ABANDON_HOURS', 48) * 3600,

    // Lectura y escritura por flujo.
    'stream_chunk' => (int) env('NUBE_STREAM_CHUNK', 65536),

    // Peticiones por minuto (limitación con la caché de la base de datos).
    'rate' => [
        'instance_per_minute' => (int) env('NUBE_RATE_INSTANCE_PER_MINUTE', 600),
        'public_per_minute' => (int) env('NUBE_RATE_PUBLIC_PER_MINUTE', 120),
    ],

    // Proxies de confianza para la IP del cliente (lista separada por comas,
    // "*" para todos, vacío para ninguno). Se verifica en el hosting real.
    'trusted_proxies' => env('NUBE_TRUSTED_PROXIES', ''),
];
