<?php

namespace Tests\Feature;

use App\Models\Link;
use Illuminate\Foundation\Testing\RefreshDatabase;
use Tests\Concerns\LinkFixtures;
use Tests\Concerns\SignedRequests;
use Tests\TestCase;

class RangeTest extends TestCase
{
    use LinkFixtures, RefreshDatabase, SignedRequests;

    private const PART = 16;

    private const SIZE = 16 * 7 + 5; // 8 partes, la última de 5 bytes

    private string $t;

    private string $pkg;

    protected function setUp(): void
    {
        parent::setUp();
        $this->makeInstance();
        // Partes pequeñas y trozos de lectura más pequeños aún, para que un
        // intervalo cruce varias partes y varios trozos.
        config(['capataz.limits.min_part_size' => self::PART, 'capataz.stream_chunk' => 5]);
        [$body, $this->pkg] = $this->activeLink(self::SIZE, self::PART);
        $this->t = $this->tokens[$body['linkId']];
    }

    private function range(string $spec, string $method = 'GET')
    {
        return $this->call($method, "/s/{$this->t}/blob", [], [], [], ['HTTP_RANGE' => $spec]);
    }

    private function assertSlice(int $a, int $b): void
    {
        $res = $this->range("bytes=$a-$b")->assertStatus(206);
        $this->assertSame(substr($this->pkg, $a, $b - $a + 1), $res->streamedContent(), "bytes=$a-$b");
        $res->assertHeader('Content-Range', "bytes $a-$b/".self::SIZE)->assertHeader('Content-Length', (string) ($b - $a + 1));
    }

    public function test_part_boundaries(): void
    {
        $last = self::SIZE - 1;
        for ($k = 1; $k * self::PART < self::SIZE; $k++) {
            $edge = $k * self::PART;
            $this->assertSlice($edge - 1, $edge - 1); // último byte de una parte
            $this->assertSlice($edge, $edge);         // primero de la siguiente
            $this->assertSlice($edge - 1, $edge);     // cruza la frontera
            $this->assertSlice($edge - 1, min($edge + self::PART, $last)); // tres partes
        }
        $this->assertSlice(0, $last);
        $this->assertSlice($last, $last);
        $this->assertSlice(self::PART * 7, $last); // la última parte, corta
    }

    public function test_random_ranges_match_the_package(): void
    {
        mt_srand(20261019);
        for ($i = 0; $i < 500; $i++) {
            $a = mt_rand(0, self::SIZE - 1);
            $b = mt_rand($a, self::SIZE - 1);
            switch ($i % 3) {
                case 0:
                    $spec = "bytes=$a-$b";
                    break;
                case 1:
                    $spec = "bytes=$a-";
                    $b = self::SIZE - 1;
                    break;
                default:
                    $n = mt_rand(1, self::SIZE + 20);
                    $spec = "bytes=-$n";
                    $a = max(self::SIZE - $n, 0);
                    $b = self::SIZE - 1;
            }
            $res = $this->range($spec);
            $this->assertSame(206, $res->getStatusCode(), $spec);
            $this->assertSame(substr($this->pkg, $a, $b - $a + 1), $res->streamedContent(), $spec);
            $this->assertSame("bytes $a-$b/".self::SIZE, $res->headers->get('Content-Range'), $spec);
        }
    }

    public function test_unsatisfiable(): void
    {
        foreach (['bytes='.self::SIZE.'-', 'bytes=0-1,4-5', 'bytes=5-3', 'items=0-1', 'bytes=-0'] as $spec) {
            $this->range($spec)->assertStatus(416)->assertJson(['code' => 'invalid_request'])
                ->assertHeader('Content-Range', 'bytes */'.self::SIZE);
        }
        $this->assertSame(0, Link::first()->downloads, 'un 416 no cuenta');
    }

    public function test_counting_and_head(): void
    {
        $this->range('bytes=10-')->assertStatus(206)->streamedContent();
        $this->range('bytes=-5')->assertStatus(206)->streamedContent(); // empieza en 112
        $this->assertSame(0, Link::first()->downloads, 'las continuaciones no cuentan');
        $this->range('bytes=0-0')->assertStatus(206)->streamedContent();
        $this->range('bytes=-500')->assertStatus(206)->streamedContent(); // sufijo que abarca todo: empieza en 0
        $this->assertSame(2, Link::first()->downloads);

        // HEAD: mismo estado y cabeceras, sin cuerpo, sin contar.
        $get = $this->range('bytes=20-40');
        $head = $this->range('bytes=20-40', 'HEAD')->assertStatus(206);
        $this->assertSame('', $head->getContent());
        foreach (['Content-Range', 'Content-Length', 'Accept-Ranges', 'Content-Type'] as $h) {
            $this->assertSame($get->headers->get($h), $head->headers->get($h), $h);
        }
        $this->range('bytes=0-', 'HEAD')->assertStatus(206);
        $this->range('bytes=99999-', 'HEAD')->assertStatus(416);
        $this->assertSame(2, Link::first()->downloads);
        $this->call('HEAD', "/s/{$this->t}/meta")->assertOk();
    }

    public function test_exhausted_continuations_within_grace(): void
    {
        Link::query()->update(['max_downloads' => 1]);
        $this->range('bytes=0-49')->assertStatus(206)->streamedContent(); // la única descarga
        $this->assertSame(Link::EXHAUSTED, Link::first()->state);

        $this->range('bytes=0-')->assertStatus(410)->assertJson(['code' => 'downloads_exhausted']);
        $this->range('bytes=0-', 'HEAD')->assertStatus(410);
        $res = $this->range('bytes=50-')->assertStatus(206); // continuación
        $this->assertSame(substr($this->pkg, 50), $res->streamedContent());
        $this->range('bytes=50-', 'HEAD')->assertStatus(206);

        $this->travel(61)->minutes();
        $this->range('bytes=50-')->assertStatus(410)->assertJson(['code' => 'downloads_exhausted']);
        $this->range('bytes=50-', 'HEAD')->assertStatus(410);
    }
}
