<?php

namespace Tests\Feature;

use App\Models\Instance;
use App\Support\Signature;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Illuminate\Http\Request;
use Illuminate\Support\Carbon;
use Illuminate\Support\Facades\Route;
use Tests\TestCase;
use Tests\Unit\SignatureVectorsTest;

class SignatureMiddlewareTest extends TestCase
{
    use RefreshDatabase;

    private array $vectors;

    protected function setUp(): void
    {
        parent::setUp();
        $this->vectors = json_decode(file_get_contents($this->docsPath('vectores-nube.json')), true)['signature'];
        Instance::create(['instance_id' => $this->vectors['instanceId'], 'key' => $this->vectors['instanceKeyBase64url']]);
        // Rutas de prueba con los destinos de los vectores: devuelven lo que
        // vio el middleware.
        $echo = fn (Request $r) => response()->json([
            'instance' => $r->attributes->get('instance')->instance_id,
            'spool' => $r->attributes->get('spool')['size'] ?? null,
        ]);
        Route::middleware('capataz.signed')->group(function () use ($echo) {
            Route::get('/v1/links', $echo);
            Route::post('/v1/links', $echo);
            Route::put('/v1/links/{id}/parts/{n}', $echo);
        });
    }

    /** Envía el caso tal cual (headers, destino crudo y cuerpo). */
    private function sendCase(array $c, array $override = [], ?string $body = null)
    {
        $headers = array_merge($c['headers'], $override);
        $server = [];
        foreach ($headers as $k => $v) {
            $server['HTTP_'.strtoupper(str_replace('-', '_', $k))] = $v;
        }
        $body ??= SignatureVectorsTest::body($c);
        $server['CONTENT_LENGTH'] = (string) strlen($body);
        $server['REQUEST_URI'] = $c['target'];
        if ($c['bodyEncoding'] === 'utf8') {
            $server['CONTENT_TYPE'] = 'application/json';
        }

        return $this->call($c['method'], $c['target'], [], [], [], $server, $body);
    }

    private function at(string $ts): void
    {
        Carbon::setTestNow(Carbon::createFromTimestamp((int) $ts));
    }

    public function test_vector_requests_pass_once(): void
    {
        foreach ($this->vectors['cases'] as $c) {
            $this->at($c['timestamp']);
            $res = $this->sendCase($c);
            $res->assertOk()->assertJson(['instance' => $this->vectors['instanceId']]);
            if ($c['method'] === 'PUT') {
                $this->assertSame(strlen(SignatureVectorsTest::body($c)), $res->json('spool'));
            }
            // Mismo nonce otra vez: replayed_nonce.
            $this->sendCase($c)->assertStatus(401)->assertJson(['code' => 'replayed_nonce']);
        }
        // El temporal de la parte no queda en disco.
        $this->assertSame([], glob($this->dataDir.'/tmp/*') ?: []);
    }

    public function test_rejections_in_contract_order(): void
    {
        $c = $this->vectors['cases'][1]; // POST con JSON
        $this->at($c['timestamp']);

        $this->sendCase($c, ['X-Capataz-Instance' => 'otra-instancia-00'])->assertStatus(401)->assertJson(['code' => 'unknown_instance']);
        Instance::where('instance_id', $this->vectors['instanceId'])->update(['active' => false]);
        $this->sendCase($c)->assertStatus(401)->assertJson(['code' => 'unknown_instance']);
        Instance::where('instance_id', $this->vectors['instanceId'])->update(['active' => true]);

        $this->sendCase($c, ['X-Capataz-Nonce' => 'corto'])->assertStatus(401)->assertJson(['code' => 'unauthorized']);

        // Reloj: 301 s de diferencia, y serverTime en la respuesta.
        Carbon::setTestNow(Carbon::createFromTimestamp((int) $c['timestamp'] + 301));
        $this->sendCase($c)->assertStatus(401)->assertJson(['code' => 'timestamp_out_of_window', 'serverTime' => (int) $c['timestamp'] + 301]);
        Carbon::setTestNow(Carbon::createFromTimestamp((int) $c['timestamp'] - 300));
        // Cuerpo alterado con la firma original: checksum_mismatch antes que invalid_signature.
        $this->sendCase($c, [], $c['body'].' ')->assertStatus(400)->assertJson(['code' => 'checksum_mismatch']);

        // Firma de otra clave (cuerpo correcto).
        $bad = Signature::sign(random_bytes(32), $c['canonical']);
        $this->sendCase($c, ['X-Capataz-Signature' => $bad])->assertStatus(401)->assertJson(['code' => 'invalid_signature']);

        // El destino se firma tal como viaja: otra consulta invalida la firma.
        $get = $this->vectors['cases'][0];
        $this->at($get['timestamp']);
        $moved = $get;
        $moved['target'] = '/v1/links?limit=2&state=active';
        $this->sendCase($moved)->assertStatus(401)->assertJson(['code' => 'invalid_signature']);

        // Una firma rechazada no gasta el nonce: la petición buena pasa después.
        $this->sendCase($get)->assertOk();
        $this->at($c['timestamp']);
        $this->sendCase($c)->assertOk();
    }

    public function test_body_limits(): void
    {
        $c = $this->vectors['cases'][2]; // PUT
        $this->at($c['timestamp']);
        config(['capataz.limits.max_part_size' => 16]);
        $this->sendCase($c)->assertStatus(413)->assertJson(['code' => 'payload_too_large']);
        $this->assertSame([], glob($this->dataDir.'/tmp/*') ?: []);
    }
}
