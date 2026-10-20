<?php

namespace App\Models;

use Illuminate\Database\Eloquent\Model;

/**
 * Parte recibida de un paquete. Los bytes están en
 * CAPATAZ_DATA_DIR/links/<linkId>/pNNNNNN.bin.
 *
 * @property int $link_id
 * @property int $part_number
 * @property int $size
 * @property string $sha256
 */
class LinkPart extends Model
{
    protected $primaryKey = null;

    public $incrementing = false;

    protected $guarded = [];

    protected function casts(): array
    {
        return ['part_number' => 'integer', 'size' => 'integer'];
    }
}
