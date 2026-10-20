<?php

namespace Tests\Feature;

use Tests\TestCase;

class ErrorsTest extends TestCase
{
    public function test_unknown_route_is_a_cloud_error(): void
    {
        $res = $this->get('/no-existe');
        $res->assertStatus(404)
            ->assertHeader('Cache-Control', 'no-store, private')
            ->assertJsonStructure(['code', 'message', 'requestId'])
            ->assertJson(['code' => 'not_found']);
        $this->assertSame($res->json('requestId'), $res->headers->get('X-Request-Id'));
        $this->assertSame([], $res->headers->getCookies(), 'sin cookies');
    }

    public function test_storage_is_not_published(): void
    {
        $this->get('/storage/x')->assertStatus(404);
        $this->put('/storage/x', [], ['Content-Type' => 'application/octet-stream'])->assertStatus(404);
    }

    public function test_limits_come_from_config(): void
    {
        $this->assertSame(524296032, config('capataz.limits.max_encrypted_bytes'));
        $this->assertSame(8388608, config('capataz.limits.max_part_size'));
        $this->assertSame(48 * 3600, config('capataz.upload_abandon_seconds'));
    }
}
