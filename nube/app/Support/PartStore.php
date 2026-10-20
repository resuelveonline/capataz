<?php

namespace App\Support;

use App\Models\Link;

/**
 * Bytes de los paquetes, por partes sin ensamblar:
 * CAPATAZ_DATA_DIR/links/<linkId>/p000001.bin, p000002.bin...
 *
 * Cada parte se publica con rename desde un temporal del mismo sistema de
 * archivos, así que reemplazarla es atómico.
 */
final class PartStore
{
    public static function dir(Link $link): string
    {
        return config('capataz.data_dir').'/links/'.$link->link_id;
    }

    public static function partPath(Link $link, int $n): string
    {
        return self::dir($link).sprintf('/p%06d.bin', $n);
    }

    /** Publica el temporal $spoolPath como parte $n (sustituye la anterior). */
    public static function put(Link $link, int $n, string $spoolPath): void
    {
        $dir = self::dir($link);
        if (! is_dir($dir) && ! @mkdir($dir, 0700, true) && ! is_dir($dir)) {
            throw new CloudError(500, 'internal_error', 'error interno');
        }
        if (! @rename($spoolPath, self::partPath($link, $n))) {
            throw new CloudError(500, 'internal_error', 'error interno');
        }
    }

    /** Borra todas las partes del enlace. */
    public static function purge(Link $link): void
    {
        $dir = self::dir($link);
        foreach (glob($dir.'/*') ?: [] as $f) {
            @unlink($f);
        }
        @rmdir($dir);
    }
}
