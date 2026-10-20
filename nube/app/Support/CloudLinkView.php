<?php

namespace App\Support;

use App\Models\Link;
use Illuminate\Support\Carbon;

/** Esquema CloudLink del contrato. Nunca incluye tokenHash ni metaBlob. */
final class CloudLinkView
{
    public static function of(Link $link): array
    {
        return [
            'linkId' => $link->link_id,
            'state' => $link->state,
            'createdAt' => self::date($link->created_at),
            'activatedAt' => self::date($link->activated_at),
            'expiresAt' => self::date($link->expires_at),
            'encryptedSize' => $link->encrypted_size,
            'partSize' => $link->part_size,
            'partCount' => $link->partCount(),
            'receivedParts' => $link->parts()->orderBy('part_number')->pluck('part_number')->map(fn ($n) => (int) $n)->all(),
            'downloads' => $link->downloads,
            'maxDownloads' => $link->max_downloads,
            'failureCode' => $link->state === Link::FAILED ? $link->failure_code : null,
        ];
    }

    /** RFC 3339 en UTC, sin fracciones: 2026-10-19T12:00:00Z. */
    public static function date(?Carbon $d): ?string
    {
        return $d?->copy()->utc()->format('Y-m-d\TH:i:s\Z');
    }
}
