<?php

namespace Tests\Concerns;

use App\Models\Instance;
use App\Support\Signature;
use Illuminate\Testing\TestResponse;

/** Peticiones firmadas como las hace Capataz (para pruebas de la API). */
trait SignedRequests
{
    protected Instance $instance;

    protected string $instanceKey;

    protected function makeInstance(string $id = 'instancia-pruebas-0001'): Instance
    {
        $this->instanceKey = random_bytes(32);

        return $this->instance = Instance::create(['instance_id' => $id, 'key' => Signature::b64url($this->instanceKey)]);
    }

    /** $target incluye la consulta tal como se envía. */
    protected function signed(string $method, string $target, string $body = '', ?Instance $as = null, ?string $key = null): TestResponse
    {
        $as ??= $this->instance;
        $key ??= $this->instanceKey;
        $ts = (string) now()->getTimestamp();
        $nonce = Signature::b64url(random_bytes(16));
        $hex = hash('sha256', $body);
        $sig = Signature::sign($key, Signature::canonical($method, $target, $ts, $nonce, $hex));
        $server = [
            'HTTP_X_CAPATAZ_INSTANCE' => $as->instance_id,
            'HTTP_X_CAPATAZ_TIMESTAMP' => $ts,
            'HTTP_X_CAPATAZ_NONCE' => $nonce,
            'HTTP_X_CAPATAZ_CONTENT_SHA256' => $hex,
            'HTTP_X_CAPATAZ_SIGNATURE' => $sig,
            'CONTENT_LENGTH' => (string) strlen($body),
            'REQUEST_URI' => $target,
            'CONTENT_TYPE' => $method === 'PUT' ? 'application/octet-stream' : 'application/json',
        ];

        return $this->call($method, $target, [], [], [], $server, $body);
    }

    protected function signedJson(string $method, string $target, array $data): TestResponse
    {
        return $this->signed($method, $target, json_encode($data, JSON_UNESCAPED_SLASHES));
    }
}
