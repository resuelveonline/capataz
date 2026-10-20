<?php

namespace App\Services;

use App\Models\Instance;
use App\Models\Link;

/** Uso de la instancia (C4): enlaces en uploading o active y sus bytes. */
final class Quota
{
    /** @return array{activeLinks: int, storedBytes: int} */
    public static function usage(Instance $instance): array
    {
        // Aplicar primero lo que ya cambió por tiempo, para no contarlo.
        Lifecycle::refreshDue($instance->id);

        $q = Link::where('instance_id', $instance->id)->whereIn('state', [Link::UPLOADING, Link::ACTIVE]);

        return ['activeLinks' => (clone $q)->count(), 'storedBytes' => (int) (clone $q)->sum('encrypted_size')];
    }

    /** @return array<string, int> esquema CloudLimits */
    public static function limits(): array
    {
        $l = config('capataz.limits');

        return [
            'maxEncryptedBytes' => $l['max_encrypted_bytes'],
            'minPartSize' => $l['min_part_size'],
            'maxPartSize' => $l['max_part_size'],
            'minTtlSeconds' => $l['min_ttl_seconds'],
            'maxTtlSeconds' => $l['max_ttl_seconds'],
            'maxActiveLinks' => $l['max_active_links'],
            'maxStoredBytes' => $l['max_stored_bytes'],
        ];
    }
}
