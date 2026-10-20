<?php

namespace App\Http\Controllers;

use App\Services\Quota;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;

class InstanceController
{
    /** GET /v1/instance: credenciales, reloj, límites y uso. */
    public function show(Request $request): JsonResponse
    {
        $instance = $request->attributes->get('instance');

        return response()->json([
            'instanceId' => $instance->instance_id,
            'serverTime' => now()->getTimestamp(),
            'limits' => Quota::limits(),
            'usage' => Quota::usage($instance),
        ], 200, ['Cache-Control' => 'no-store']);
    }
}
