<?php

namespace App\Support;

use App\Models\Link;

/**
 * Sirve [start, end] (inclusive) de un paquete guardado por partes: traduce
 * el desplazamiento a (parte, desplazamiento dentro de la parte) y lee en
 * trozos de NUBE_STREAM_CHUNK, pasando de una parte a la siguiente. Nunca
 * carga una parte entera ni usa readfile.
 */
final class BlobStreamer
{
    /** @param  callable(string): void  $emit */
    public static function stream(Link $link, int $start, int $end, callable $emit): void
    {
        $partSize = $link->part_size;
        $chunk = max(1, (int) config('capataz.stream_chunk'));
        $pos = $start;
        while ($pos <= $end) {
            $n = intdiv($pos, $partSize) + 1;
            $offset = $pos % $partSize;
            $fh = @fopen(PartStore::partPath($link, $n), 'rb');
            if ($fh === false || fseek($fh, $offset) !== 0) {
                throw new \RuntimeException('parte ilegible'); // sin rutas ni nombres
            }
            // Hasta el final de esta parte o del intervalo, lo que llegue antes.
            $left = min($link->expectedPartSize($n) - $offset, $end - $pos + 1);
            while ($left > 0) {
                $buf = fread($fh, min($chunk, $left));
                if ($buf === false || $buf === '') {
                    fclose($fh);
                    throw new \RuntimeException('parte truncada');
                }
                $emit($buf);
                $left -= strlen($buf);
                $pos += strlen($buf);
            }
            fclose($fh);
        }
    }
}
