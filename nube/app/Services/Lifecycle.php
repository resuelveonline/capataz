<?php

namespace App\Services;

use App\Models\Link;
use App\Support\PartStore;

/**
 * Transiciones de estado que dependen del tiempo. Se aplican al leer un
 * enlace (así la respuesta nunca está atrasada) y en la pasada del
 * programador.
 *
 *  - uploading, active o exhausted con expires_at pasado: expired, sin bytes;
 *  - exhausted fuera del margen NUBE_EXHAUSTED_GRACE: sigue exhausted, sin bytes;
 *  - uploading sin actividad durante NUBE_UPLOAD_ABANDON_HOURS (desde la
 *    última parte o la apertura): failed con upload_abandoned, sin bytes.
 */
final class Lifecycle
{
    public static function refresh(Link $link): Link
    {
        $now = now();
        $changed = false;
        if (in_array($link->state, [Link::UPLOADING, Link::ACTIVE, Link::EXHAUSTED], true) && $now->gte($link->expires_at)) {
            $link->state = Link::EXPIRED;
            $changed = true;
        } elseif ($link->state === Link::UPLOADING
            && $now->gte($link->last_activity_at->copy()->addSeconds(config('capataz.upload_abandon_seconds')))) {
            $link->state = Link::FAILED;
            $link->failure_code = 'upload_abandoned';
            $changed = true;
        }
        $keepBytes = $link->state === Link::UPLOADING || $link->state === Link::ACTIVE
            || ($link->state === Link::EXHAUSTED && $now->lt($link->exhausted_at->copy()->addSeconds(config('capataz.exhausted_grace'))));
        if (! $keepBytes && $link->purged_at === null) {
            self::purge($link);
            $changed = true;
        }
        if ($changed) {
            $link->save();
        }

        return $link;
    }

    /** Borra los bytes y las partes registradas. */
    public static function purge(Link $link): void
    {
        PartStore::purge($link);
        $link->parts()->delete();
        $link->purged_at = now();
    }
}
