# opencode-free-proxy

Proxy local en Go que expone una API compatible con OpenAI y la resuelve contra **backends 100% gratuitos**, con prioridad en el tier free de [opencode Zen](https://opencode.ai/zen) (`big-pickle` y amigos). Cero costo, cero tokens facturados.

## Características

- ✅ API compatible con OpenAI: `POST /v1/chat/completions` y `GET /v1/models`
- 🆓 Provider **zen** sin API key (usa `Bearer public` + markers del cliente oficial)
- 🔒 Provider **serve**: tunnelea a través de un `opencode serve` local (`127.0.0.1:8377`), la vía infalible que siempre pasa los chequeos del vendor
- 🔄 Auto-descubrimiento: sincroniza el catálogo free desde `models.dev` en cada arranque
- 🚦 Rate-limiting por provider (token bucket) y timeouts configurables
- 🔐 Auth opcional por token (`config.json` → `server.token`)
- 🌍 CORS habilitado para uso desde navegadores
- 🧪 `--selftest` para verificar que la receta sigue funcionando

## Providers incluidos

| Provider | API Key | Modelos |
|----------|---------|---------|
| `serve` | No | vía `opencode serve` local (big-pickle) |
| `zen` | No (`public`) | big-pickle, minimax-m2.1-free, deepseek-v4-flash-free, nemotron-3-ultra-free |
| `openrouter` | `OPENROUTER_API_KEY` | deepseek-r1:free, llama-3.3-70b:free, qwen-2.5-72b:free, etc. |
| `groq` | `GROQ_API_KEY` | llama-3.3-70b-versatile, llama-3.1-8b-instant |
| `gemini` | `GEMINI_API_KEY` | gemini-2.5-flash, gemini-2.0-flash |

## Uso

```bash
# compilar
go build -o opencode-api .

# correr con defaults (escucha en 127.0.0.1:8080)
./opencode-api

# con config y listen custom
./opencode-api -config config.json -listen 0.0.0.0:8080

# selftest: prueba la receta clonada contra zen y sale
./opencode-api -selftest
```

## Ejemplos

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "big-pickle",
    "messages": [{"role": "user", "content": "hola mundo"}]
  }'
```

Con streaming:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "pickle",
    "stream": true,
    "messages": [{"role": "user", "content": "cuéntame un chiste"}]
  }'
```

Listar modelos disponibles:

```bash
curl http://127.0.0.1:8080/v1/models
```

## Aliases

Puedes usar nombres cortos en el campo `model`:

- `pickle`, `big-pickle`, `free` → serve/zen con big-pickle
- `zen` → provider zen directo
- `r1` → deepseek-r1:free (openrouter)
- `deepseek` → deepseek-chat:free
- `llama` → llama-3.3-70b:free
- `llama-g` → groq llama-3.3-70b
- `flash`, `flash-lite` → gemini

## Configuración

El archivo `config.json` es **opcional** — sin él, los defaults ya funcionan para los providers keyless (`serve` y `zen`). Copia `config.example.json` si quieres personalizar:

```bash
cp config.example.json config.json
```

Los API keys se resuelven desde variables de entorno (`OPENROUTER_API_KEY`, `GROQ_API_KEY`, `GEMINI_API_KEY`) o del campo `api_key` en el JSON.

## Arquitectura

```
main.go       → entrypoint, flags, graceful shutdown
config.go     → config + merge de defaults + resolución de keys
router.go     → resolución modelo→provider, token bucket rate-limit
server.go     → HTTP server, auth, CORS, forward/SSE
mimic.go      → fingerprint TLS tipo Bun para imitar al cliente oficial
zen.go        → provider zen + enrich dinámico desde models.dev
serve.go      → lane "serve": sesión/mensaje vía opencode serve local
selftest.go   → probe end-to-end
```

## Nota

El tier free de Zen está walled al cliente oficial de opencode. Este proxy habla su mismo dialecto (User-Agent, headers `x-opencode-*`, fingerprint TLS de Bun) para poder usarlo desde cualquier cliente OpenAI-compatible.