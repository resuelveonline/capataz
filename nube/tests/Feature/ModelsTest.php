<?php

namespace Tests\Feature;

use App\Models\Instance;
use App\Models\Link;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Illuminate\Support\Facades\DB;
use Tests\TestCase;

class ModelsTest extends TestCase
{
    use RefreshDatabase;

    public function test_instance_key_is_encrypted_at_rest(): void
    {
        $raw = random_bytes(32);
        $b64 = rtrim(strtr(base64_encode($raw), '+/', '-_'), '=');
        $inst = Instance::create(['instance_id' => 'instancia-modelo-01', 'key' => $b64]);

        $stored = DB::table('instances')->where('id', $inst->id)->value('key');
        $this->assertStringNotContainsString($b64, $stored);
        $this->assertSame($raw, Instance::find($inst->id)->rawKey());
        $this->assertArrayNotHasKey('key', $inst->toArray());
    }

    public function test_part_sizes(): void
    {
        $l = new Link(['encrypted_size' => 2 * 1048576 + 5, 'part_size' => 1048576]);
        $this->assertSame(3, $l->partCount());
        $this->assertSame(1048576, $l->expectedPartSize(1));
        $this->assertSame(5, $l->expectedPartSize(3));

        $exact = new Link(['encrypted_size' => 2 * 1048576, 'part_size' => 1048576]);
        $this->assertSame(2, $exact->partCount());
        $this->assertSame(1048576, $exact->expectedPartSize(2));
    }

    public function test_nonce_primary_key_rejects_duplicates(): void
    {
        $inst = Instance::create(['instance_id' => 'instancia-modelo-02', 'key' => 'x']);
        $row = ['instance_id' => $inst->id, 'nonce' => str_repeat('A', 22), 'seen_at' => now()];
        DB::table('nonces')->insert($row);
        $this->expectException(\Illuminate\Database\UniqueConstraintViolationException::class);
        DB::table('nonces')->insert($row);
    }
}
