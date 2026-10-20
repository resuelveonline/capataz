<?php

namespace Tests\Unit;

use App\Support\RangeParser;
use PHPUnit\Framework\TestCase;

/** docs/vectores-range.json: los mismos casos que prueba nubetest (Go). */
class RangeParserTest extends TestCase
{
    public function test_vectors(): void
    {
        $d = json_decode(file_get_contents(dirname(__DIR__, 3).'/docs/vectores-range.json'), true, flags: JSON_THROW_ON_ERROR);
        $this->assertGreaterThanOrEqual(30, count($d['cases']));
        foreach ($d['cases'] as $c) {
            $r = RangeParser::parse($c['headers'], $d['size']);
            $status = $r === null ? 416 : ($r[2] ? 206 : 200);
            $label = json_encode($c['headers']);
            $this->assertSame($c['status'], $status, $label);
            if ($r !== null) {
                $this->assertSame([$c['start'], $c['end']], [$r[0], $r[1]], $label);
            }
        }
    }

    public function test_php_joins_repeated_headers(): void
    {
        // Dos headers Range llegan a PHP como uno solo unido con ", ".
        $this->assertNull(RangeParser::parse(['bytes=0-5, bytes=6-9'], 100));
    }
}
