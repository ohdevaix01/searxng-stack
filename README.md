# searxng-stack

Buscador propio con API JSON, listo para VPS chico.

SearXNG hace el trabajo de buscar. Una capa API mínima encima le agrega lo que falta:
límite de resultados, caché en SQLite, CORS y endpoints de salud.

## Qué resuelve

- **20 resultados por búsqueda** por defecto (`limit` configurable, tope 50).
- **Caché** en disco: la misma búsqueda no vuelve a salir a la red.
- **Sin API key de nadie.** SearXNG scrapea resultados públicos.
- **Liviano**: ~140 MB de RAM en total.

## Consumo real medido

| Contenedor | Imagen | RAM |
|---|---|---|
| searxng | 404 MB | ~124 MB |
| api | 74 MB | ~16 MB |

## Arrancar

```bash
cp .env.example .env
# editar .env: poner una SEARXNG_SECRET larga (openssl rand -hex 32)
docker compose up -d
```

Queda escuchando en `http://TU_IP:8080`.

```bash
curl "http://TU_IP:8080/search?q=clubes+san+luis&limit=20"
```

Abrí `http://TU_IP:8080/` en el navegador para ver la demo.

## API

### `GET /search`

| Param | Default | Descripción |
|---|---|---|
| `q` | — | Obligatorio. Texto a buscar. |
| `limit` | 20 | Resultados a devolver. Máximo 50. |
| `lang` | es | Idioma de los resultados. |
| `category` | — | `general`, `news`, `images`, `it`, etc. |
| `engines` | — | Forzar motores: `bing,brave`. |
| `time_range` | — | `day`, `week`, `month`, `year`. |
| `pageno` | 1 | Página. |

Respuesta:

```json
{
  "query": "clubes san luis",
  "count": 20,
  "limit": 20,
  "results": [
    {
      "url": "https://...",
      "title": "Título del resultado",
      "content": "Extracto...",
      "engine": "brave"
    }
  ],
  "suggestions": [],
  "unresponsive_engines": [],
  "cached": false,
  "took_ms": 1840
}
```

El header `X-Cache` devuelve `HIT` o `MISS`.

### `GET /health`

```json
{"status": "ok", "cache_ttl": 3600}
```

### `GET /stats`

```json
{"requests": 42, "cache_hits": 31, "errors": 0}
```

## Variables de entorno

Todas en `.env`. Ver `.env.example`.

| Variable | Default | Qué hace |
|---|---|---|
| `API_PORT` | 8080 | Puerto público de la API. |
| `DEFAULT_LIMIT` | 20 | Resultados si el cliente no pide otro número. |
| `MAX_LIMIT` | 50 | Tope duro. |
| `CACHE_TTL` | 3600 | Segundos que dura una entrada de caché. |
| `API_KEY` | vacío | Si se pone, exige `?key=` o header `X-API-Key`. |
| `UPSTREAM_TIMEOUT` | 25 | Segundos máximos esperando a SearXNG. |
| `SEARXNG_SECRET` | — | Clave interna de SearXNG. **Cambiar en producción.** |

## Motores

Activos: `google cse`, `bing`, `duckduckgo`, `brave`, `mojeek`, `startpage`, `wikipedia`.

`google` directo queda apagado porque desde IP de datacenter devuelve *access denied*.
Si tu VPS tiene IP residencial, podés probar habilitarlo en `searxng/settings.yml`.

## Cómo funciona el scraping

SearXNG no tiene índice propio. Pide la página de resultados a cada motor, la parsea y
devuelve todo junto. No usa las APIs oficiales, así que no hay key ni contrato.

Consecuencia: los motores pueden bloquear por IP. No es permanente — es un castigo
temporal que se levanta solo. A volumen bajo (decenas o cientos por día) no pasa nada.
A volumen alto conviene proxy residencial rotativo.

El `settings.yml` tiene los tiempos de suspensión bajados a propósito, para que un motor
caído se reintente en minutos en vez de quedar horas afuera.

## Estructura

```
.
├── api/
│   ├── app.py          # API (solo stdlib, sin dependencias)
│   └── Dockerfile
├── searxng/
│   └── settings.yml    # motores, timeouts, límites
├── public/
│   └── index.html      # demo
├── docker-compose.yml
└── .env.example
```

## Notas

- SearXNG no publica su puerto: solo la API lo alcanza por la red interna de Docker.
- La caché vive en el volumen `cache-data`, sobrevive a los reinicios.
- `api/app.py` no tiene dependencias externas, así que la imagen es Alpine + Python y nada más.
