<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;
use Illuminate\Database\Eloquent\Relations\HasMany;

/**
 * Instancia de Capataz dada de alta con capataz:instance:add.
 *
 * @property int $id
 * @property string $instance_id
 * @property string $key clave HMAC en base64url (descifrada por el cast)
 * @property bool $active
 */
class Instance extends Model
{
    protected $fillable = ['instance_id', 'key', 'active'];

    // La clave nunca sale en toArray/toJson.
    protected $hidden = ['key'];

    protected function casts(): array
    {
        return ['key' => 'encrypted', 'active' => 'boolean'];
    }

    /** Los 32 bytes de la clave HMAC. */
    public function rawKey(): string
    {
        return base64_decode(strtr($this->key, '-_', '+/'), true) ?: '';
    }

    public function links(): HasMany
    {
        return $this->hasMany(Link::class);
    }
}
