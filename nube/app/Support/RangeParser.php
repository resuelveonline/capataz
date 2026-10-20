<?php

namespace App\Support;

/**
 * Range de /s/{token}/blob (contrato v0.1.1), con las mismas reglas que
 * nubetest.ParseRange y los casos de docs/vectores-range.json:
 *
 *  - sin header: el paquete entero (200);
 *  - un solo intervalo "bytes=a-b", "bytes=a-" o "bytes=-n" (unidad sin
 *    distinguir mayúsculas, espacios alrededor y tras "=" permitidos); b
 *    se recorta al último byte; n mayor que el paquete es el paquete entero;
 *  - 416 si hay varios intervalos o varios headers (PHP los une con ", "),
 *    otra unidad, sintaxis inválida, a > b, a fuera del paquete o n = 0.
 */
final class RangeParser
{
    /**
     * @param  list<string>  $values  valores del header Range (vacío = sin header)
     * @return array{0: int, 1: int, 2: bool}|null [start, end, parcial] o null si es 416
     */
    public static function parse(array $values, int $size): ?array
    {
        if ($values === []) {
            return [0, $size - 1, false];
        }
        if (count($values) > 1) {
            return null;
        }
        $v = trim($values[0]);
        if (strlen($v) < 6 || strcasecmp(substr($v, 0, 6), 'bytes=') !== 0 || str_contains($v, ',')) {
            return null;
        }
        $spec = trim(substr($v, 6));
        $dash = strpos($spec, '-');
        if ($dash === false) {
            return null;
        }
        $first = substr($spec, 0, $dash);
        $last = substr($spec, $dash + 1);
        if (($first !== '' && ! ctype_digit($first)) || ($last !== '' && ! ctype_digit($last)) || ($first === '' && $last === '')) {
            return null;
        }
        if ($first === '') { // sufijo: los últimos n bytes
            $n = self::toInt($last);
            if ($n === 0) {
                return null;
            }
            $n ??= $size; // más largo que cualquier paquete: entero

            return [max($size - $n, 0), $size - 1, true];
        }
        $a = self::toInt($first);
        if ($a === null || $a >= $size) {
            return null;
        }
        $b = $size - 1;
        if ($last !== '') {
            $v = self::toInt($last);
            if ($v !== null) {
                if ($a > $v) {
                    return null;
                }
                $b = min($v, $size - 1);
            } // desbordado: hasta el final, como en Go
        }

        return [$a, $b, true];
    }

    /** Dígitos a entero; null si no cabe en 64 bits (como ErrRange en Go). */
    private static function toInt(string $digits): ?int
    {
        $d = ltrim($digits, '0');
        if ($d === '') {
            return 0;
        }
        if (strlen($d) > 19 || (strlen($d) === 19 && strcmp($d, (string) PHP_INT_MAX) > 0)) {
            return null;
        }

        return (int) $d;
    }

    /** Valores del header Range tal como llegan. */
    public static function headerValues(\Illuminate\Http\Request $request): array
    {
        return $request->headers->has('Range') ? array_values(array_map('strval', $request->headers->all('range'))) : [];
    }
}
