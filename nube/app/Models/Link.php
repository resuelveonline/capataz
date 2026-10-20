<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\BelongsTo;
use Illuminate\Database\Eloquent\Relations\HasMany;

/**
 * Enlace (CloudLink). Estados: uploading, active, expired, exhausted,
 * revoked, failed.
 *
 * @property int $id
 * @property string $link_id
 * @property int $instance_id
 * @property string $token_hash
 * @property string $state
 * @property \Illuminate\Support\Carbon $expires_at
 * @property int|null $max_downloads
 * @property int $downloads
 * @property string $cipher_version
 * @property int $encrypted_size
 * @property int $part_size
 * @property string $meta_blob
 * @property string $request_hash
 * @property string|null $package_sha256
 * @property string|null $failure_code
 * @property \Illuminate\Support\Carbon|null $activated_at
 * @property \Illuminate\Support\Carbon|null $exhausted_at
 * @property \Illuminate\Support\Carbon $last_activity_at
 * @property \Illuminate\Support\Carbon|null $purged_at
 * @property \Illuminate\Support\Carbon $created_at
 */
class Link extends Model
{
    public const UPLOADING = 'uploading';

    public const ACTIVE = 'active';

    public const EXPIRED = 'expired';

    public const EXHAUSTED = 'exhausted';

    public const REVOKED = 'revoked';

    public const FAILED = 'failed';

    public const STATES = [self::UPLOADING, self::ACTIVE, self::EXPIRED, self::EXHAUSTED, self::REVOKED, self::FAILED];

    protected $guarded = ['id'];

    // Igual que en la migración, para que un modelo recién creado lo tenga.
    protected $attributes = ['downloads' => 0];

    // tokenHash y metaBlob no se exponen salvo en /s/{token}/meta.
    protected $hidden = ['token_hash', 'meta_blob', 'request_hash'];

    protected function casts(): array
    {
        return [
            'expires_at' => 'datetime',
            'activated_at' => 'datetime',
            'exhausted_at' => 'datetime',
            'last_activity_at' => 'datetime',
            'purged_at' => 'datetime',
            'max_downloads' => 'integer',
            'downloads' => 'integer',
            'encrypted_size' => 'integer',
            'part_size' => 'integer',
        ];
    }

    public function partCount(): int
    {
        return intdiv($this->encrypted_size + $this->part_size - 1, $this->part_size);
    }

    /** Tamaño exacto de la parte $n (todas partSize salvo la última). */
    public function expectedPartSize(int $n): int
    {
        $count = $this->partCount();

        return $n < $count ? $this->part_size : $this->encrypted_size - ($count - 1) * $this->part_size;
    }

    /** Ocupa cupo (C4): en subida o activo. */
    public function occupiesQuota(): bool
    {
        return in_array($this->state, [self::UPLOADING, self::ACTIVE], true);
    }

    public function instance(): BelongsTo
    {
        return $this->belongsTo(Instance::class);
    }

    public function parts(): HasMany
    {
        return $this->hasMany(LinkPart::class);
    }
}
