<?php

namespace App\Http\Middleware;

use Closure;
use Illuminate\Http\Request;
use Symfony\Component\HttpFoundation\Response;

/**
 * Da a cada petición un identificador aleatorio (X-Request-Id y requestId
 * de CloudError) para cruzar la respuesta con la bitácora sin registrar
 * nada de la petición.
 */
class AssignRequestId
{
    public function handle(Request $request, Closure $next): Response
    {
        $id = bin2hex(random_bytes(8));
        $request->attributes->set('requestId', $id);
        $response = $next($request);
        $response->headers->set('X-Request-Id', $id);

        return $response;
    }
}
