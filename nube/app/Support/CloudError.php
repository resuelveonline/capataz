<?php

namespace App\Support;

use Illuminate\Http\JsonResponse;
use RuntimeException;

/**
 * Error del contrato (esquema CloudError): estado HTTP y código estable.
 * El mensaje es genérico y nunca lleva tokens, claves, cuerpos ni nombres.
 */
class CloudError extends RuntimeException
{
    /**
     * @param  array<string, mixed>  $extra  campos adicionales del contrato (serverTime, missingParts)
     * @param  array<string, string>  $headers
     */
    public function __construct(
        public readonly int $status,
        public readonly string $errorCode,
        string $message = '',
        public readonly array $extra = [],
        public readonly array $headers = [],
    ) {
        parent::__construct($message !== '' ? $message : $errorCode);
    }

    public function toResponse(?string $requestId): JsonResponse
    {
        $body = ['code' => $this->errorCode, 'message' => $this->getMessage()] + $this->extra;
        if ($requestId !== null) {
            $body['requestId'] = $requestId;
        }

        return response()->json($body, $this->status, $this->headers + ['Cache-Control' => 'no-store']);
    }

    public static function notFound(): self
    {
        return new self(404, 'not_found', 'no existe');
    }

    public static function invalid(string $message = 'petición inválida'): self
    {
        return new self(400, 'invalid_request', $message);
    }

    public static function conflict(string $code = 'conflict', string $message = 'conflicto de estado', array $extra = []): self
    {
        return new self(409, $code, $message, $extra);
    }
}
