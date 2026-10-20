# Capataz Nube

Servidor Laravel que recibe, guarda y sirve los paquetes cifrados CPK1 de los
enlaces temporales de Capataz. Contrato: [`../api/capataz-nube-api.yaml`](../api/capataz-nube-api.yaml)
(v0.1.1). La nube nunca ve contenido, nombres ni claves.

Estado: Fase 2c en curso. Documentación completa de despliegue al cerrar la fase.

```sh
composer install
cp .env.example .env && php artisan key:generate
php artisan migrate
composer test            # PHPUnit
```

Requisitos: PHP 8.4 (el lock se resuelve para 8.4.26, la del hosting) con
pdo_sqlite o pdo_mysql, openssl, mbstring, fileinfo, dom y tokenizer. No usa
sodium.
