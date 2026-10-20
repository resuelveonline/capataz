<?php

use App\Http\Middleware\AssignRequestId;
use App\Http\Middleware\VerifyInstanceSignature;
use App\Support\CloudError;
use Illuminate\Foundation\Application;
use Illuminate\Foundation\Configuration\Exceptions;
use Illuminate\Foundation\Configuration\Middleware;
use Illuminate\Http\Exceptions\ThrottleRequestsException;
use Illuminate\Http\Request;
use Symfony\Component\HttpKernel\Exception\HttpExceptionInterface;
use Symfony\Component\HttpKernel\Exception\MethodNotAllowedHttpException;
use Symfony\Component\HttpKernel\Exception\NotFoundHttpException;

return Application::configure(basePath: dirname(__DIR__))
    ->withRouting(
        // Solo rutas sin sesión ni cookies: /v1 (firmadas) y /s (públicas).
        api: __DIR__.'/../routes/api.php',
        apiPrefix: '',
        commands: __DIR__.'/../routes/console.php',
    )
    ->withMiddleware(function (Middleware $middleware): void {
        $middleware->prepend(AssignRequestId::class);
        $middleware->alias(['capataz.signed' => VerifyInstanceSignature::class]);
    })
    ->withExceptions(function (Exceptions $exceptions): void {
        // Los CloudError son respuestas previstas, no fallos.
        $exceptions->dontReport(CloudError::class);
        // Ninguna excepción lleva a la bitácora la petición: solo su clase
        // y mensaje (los mensajes de la aplicación no incluyen secretos).
        $exceptions->dontFlash(['*']);

        // Toda respuesta de error es un CloudError en JSON.
        $exceptions->render(function (Throwable $e, Request $request) {
            $id = $request->attributes->get('requestId');
            $err = match (true) {
                $e instanceof CloudError => $e,
                $e instanceof NotFoundHttpException => CloudError::notFound(),
                $e instanceof MethodNotAllowedHttpException => new CloudError(405, 'invalid_request', 'método no permitido',
                    headers: ['Allow' => $e->getHeaders()['Allow'] ?? '']),
                $e instanceof ThrottleRequestsException => new CloudError(429, 'rate_limited', 'demasiadas peticiones',
                    headers: ['Retry-After' => (string) ($e->getHeaders()['Retry-After'] ?? 60)]),
                $e instanceof HttpExceptionInterface && $e->getStatusCode() === 413 => new CloudError(413, 'payload_too_large', 'cuerpo demasiado grande'),
                $e instanceof HttpExceptionInterface && $e->getStatusCode() < 500 => CloudError::invalid(),
                default => new CloudError(500, 'internal_error', 'error interno'),
            };

            return $err->toResponse($id);
        });
    })->create();
