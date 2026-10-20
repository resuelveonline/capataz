<?php

namespace Tests\Feature;

use App\Models\Instance;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Illuminate\Support\Facades\Artisan;
use Illuminate\Support\Facades\DB;
use Symfony\Component\Console\Output\BufferedOutput;
use Tests\TestCase;

class InstanceCommandsTest extends TestCase
{
    use RefreshDatabase;

    public function test_add_prints_key_once_and_stores_it_encrypted(): void
    {
        $out = new BufferedOutput;
        $code = Artisan::call('capataz:instance:add', ['instanceId' => 'instancia-nueva-01'], $out);
        $this->assertSame(0, $code);
        // BufferedOutput junta stdout y stderr; en la consola real stdout
        // lleva solo la clave. Es la última línea.
        $lines = explode("\n", trim($out->fetch()));
        $key = end($lines);
        // 32 bytes en base64url (43 caracteres).
        $this->assertMatchesRegularExpression('/^[A-Za-z0-9_-]{43}$/', $key);

        $inst = Instance::where('instance_id', 'instancia-nueva-01')->firstOrFail();
        $this->assertTrue($inst->active);
        $this->assertSame($key, $inst->key);
        $this->assertSame(32, strlen($inst->rawKey()));
        $this->assertStringNotContainsString($key, (string) DB::table('instances')->value('key'));

        // Repetir no muestra ni cambia la clave.
        $out2 = new BufferedOutput;
        $this->assertSame(1, Artisan::call('capataz:instance:add', ['instanceId' => 'instancia-nueva-01'], $out2));
        $this->assertStringNotContainsString($key, $out2->fetch());
        $this->assertSame($key, Instance::first()->key);
    }

    public function test_add_rejects_invalid_ids(): void
    {
        foreach (['corto', str_repeat('a', 65), 'con espacio x', 'con/barra-00'] as $id) {
            $this->assertSame(2, Artisan::call('capataz:instance:add', ['instanceId' => $id]), $id);
        }
        $this->assertSame(0, Instance::count());
    }

    public function test_disable(): void
    {
        Artisan::call('capataz:instance:add', ['instanceId' => 'instancia-baja-01']);
        $this->assertSame(0, Artisan::call('capataz:instance:disable', ['instanceId' => 'instancia-baja-01']));
        $this->assertFalse(Instance::first()->active);
        $this->assertSame(1, Artisan::call('capataz:instance:disable', ['instanceId' => 'no-existe-000']));
    }
}
