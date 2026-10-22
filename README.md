# opencode-free-proxy

Proxy local en Go que expone una API compatible con OpenAI y la resuelve contra **backends 100% gratuitos**, con prioridad en el tier free de [opencode Zen](https://opencode.ai/zen) (`big-pickle` y amigos). Cero costo, cero tokens facturados.

## Características

- ✅ API compatible con OpenAI: `POST /v1/chat/completions` y `GET /v1/models`
- 🧬 API compatible con Anthropic: `POST /v1/messages` (+ `count_tokens`) — Claude Code funciona directo
- 🆓 Provider **zen** sin API key: `Bearer public` + headers `x-opencode-*` + la receta de dos modos que exige el gate (sin TLS falso)
- 🔒 Provider **serve**: fallback opcional vía `opencode serve` dedicado; ya no hace falta para zen
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

### Aislado del TUI (recomendado si usas `opencode` en la misma máquina)

El carril `zen` directo ya funciona solo: no necesita `opencode` ni cuenta.
`run.sh` además levanta un `opencode serve` dedicado como fallback, separado del
TUI (el carril `serve` habla `/session` y comparte base si apunta a 8377: de ahí
los cuelgues que veías en la sesión interactiva).

```bash
./run.sh    # opencode serve propio en 8399 (XDG_DATA_HOME aparte) + proxy en 8099
./stop.sh   # apaga solo lo que arrancó run.sh
```

- proxy: `127.0.0.1:8099`
- `opencode serve` dedicado (fallback): `127.0.0.1:8399`
- almacén aislado: `~/.local/share/opencode-proxy-svc` (auth.json copiado una vez)
- el primer arranque autollena la config: `pickle`/`free` → zen directo

El autostart del proxy también aísla su `serve` (`serve.go` → `proxyDataHome`),
así que aunque no uses `run.sh` no vuelve a pisar las sesiones del TUI.

### Manual

```bash
# compilar
go build -o opencode-api .

# correr con defaults (escucha en 127.0.0.1:8080)
./opencode-api

# con config y listen custom
./opencode-api -config config.json -listen 0.0.0.0:8080

# selftest: prueba zen directo y el carril serve
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

- `pickle`, `big-pickle`, `free` → zen directo con big-pickle
- `serve` → carril opencode local (fallback)
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
anthropic.go  → /v1/messages (Claude Code): traducción Anthropic↔OpenAI + SSE
ids.go        → IDs con el esquema de opencode (ses_/msg_)
zen.go        → provider zen + receta de dos modos + enrich desde models.dev
serve.go      → lane "serve": sesión/mensaje vía opencode serve local
selftest.go   → probe end-to-end
```

## Claude Code

El proxy expone `/v1/messages` (formato Anthropic), así que Claude Code corre
contra cualquiera de los dos carriles sin capas extra:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8099
export ANTHROPIC_AUTH_TOKEN=public
export ANTHROPIC_MODEL=zen/deepseek-v4.1-flash-free          # carril zen directo
# o:  ANTHROPIC_MODEL=serve/deepseek-v4.1-flash-free          # carril opencode serve
export ANTHROPIC_SMALL_FAST_MODEL=zen/deepseek-v4.1-flash-free
export ANTHROPIC_DEFAULT_HAIKU_MODEL=zen/deepseek-v4.1-flash-free
export ANTHROPIC_DEFAULT_SONNET_MODEL=zen/deepseek-v4.1-flash-free
export ANTHROPIC_DEFAULT_OPUS_MODEL=zen/deepseek-v4.1-flash-free
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1

claude -p "hola" --model zen/deepseek-v4.1-flash-free
```

El prefijo del modelo (`zen/…`, `serve/…`) elige el carril; cualquier modelo
free del catálogo sirve en ambos. El bucle de herramientas (Write, Read, Bash…)
está verificado end-to-end en los dos carriles.

## Nota

El tier free de Zen está walled al cliente oficial. Lo que valida, comprobado
contra el binario 1.18.34 (petición real capturada con un tap local) y probado
en Claude Code 2.1.291:

El gate tiene **dos modos**; mezclarlos da `FreeTierError`:

1. **Modo título** — peticiones sin tools: el system prompt debe llevar los
   fragmentos `You are a title generator. You output ONLY a thread title.
   Nothing else.` y `- Never use tools`. Con el neutralizador después, el
   modelo responde normal.
2. **Modo agente** — peticiones con tools: el banner de título **prohíbe**
   pasar (hay que quitarlo) y el set de tools debe incluir la tool oficial
   `read` de opencode con su schema exacto. Es la prueba de cliente que revisa
   el gate.

`shapeZenRequest` elige el modo solo: con tools quita el banner, inyecta la
tool `read` oficial (capturada del binario) y fija `tool_choice: auto`; sin
tools inyecta el banner de modo título. Las llamadas del modelo a la `read`
inyectada se mapean de vuelta a la `Read` del cliente antes de responder.
Si el cliente pidió `stream:false`, `aggregateSSE` rearma la respuesta JSON.

Lo demás no lo miran: endpoint `https://opencode.ai/inference/openai/v1`,
`stream: true` en el body, y headers `Authorization: Bearer public`,
User-Agent de opencode y `x-opencode-client/project/request/session/session-id`.
El TLS tampoco: la misma petición funciona sobre TLS de Go.