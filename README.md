# web-search-redirect (CLIProxyAPI plugin)

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that gives models without hosted web
search a working `web_search` tool on the OpenAI Responses API. Searches run on an OpenAI model. The answer
still comes from the model the client asked for.

OSS vendors behind `openai-compatibility` expose only chat completions, so CLIProxyAPI drops the hosted
`{"type":"web_search"}` tool that Codex and other Responses clients send. With this plugin, the OSS model
can search:

```
client ──/v1/responses──▶ plugin ──▶ OSS model (web_search as a function tool)
                            │            │ calls web_search("query")
                            │◀───────────┘
                            ├──▶ OpenAI model with hosted web_search ──▶ summary + sources
                            ├──▶ OSS model (with the results) ──▶ final answer
client ◀── one Responses stream: web_search_call items + the OSS model's answer
```

## How it works

The plugin declares a `model_router` and an `executor` for the `openai-response` format.

1. The router claims Responses requests whose model matches `models` and whose `tools` include a hosted
   web search tool (`web_search`, `web_search_preview`, ...). Everything else passes through untouched.
2. The executor replaces the hosted tool with a `web_search(query)` function tool and streams the
   request to the same model through the host. The host skips this plugin's router for nested calls,
   so the request reaches the real provider.
3. When the model calls `web_search`, the plugin hides that call from the client. It runs the query on
   `search_model` with the client's original hosted tool definition, so options such as
   `external_web_access`, `filters`, and `user_location` carry over. The model gets a summary with
   source URLs, and the loop continues until the model answers.
4. The client sees a single, well-formed Responses stream. It contains one `response.created`, a
   `web_search_call` item per search (`in_progress` → `searching` → `completed`, with `action.sources`),
   the model's own output, and a final `response.completed`. Sequence numbers and output indexes are
   renumbered, and usage is summed across turns.

Clients replay `web_search_call` items on later turns. The plugin turns them back into a function call
and output pair from an in-memory cache, so the model still sees what it found. After a proxy restart, the
cache is empty and the model gets the query and source URLs only.

If a turn calls both `web_search` and a client tool (for example `shell`), the plugin runs the search and
then ends the response. The client runs its tool as usual and replays the search on the next request.

Side endpoints such as `/responses/compact` are forwarded to the model with the hosted tool removed.

## Requirements

- CLIProxyAPI with plugin ABI 1 and RPC schema ≤ 6. This plugin is built against `v7.3.15`.
  Rebuild after upgrading CLIProxyAPI if plugin loading fails.
- Go 1.26+ with cgo.
- A provider for `search_model` that supports hosted web search on the Responses API: a Codex OAuth
  account, or a `codex-api-key` entry. An OpenAI API key works as a `codex-api-key` with
  `base-url: https://api.openai.com/v1`.
- Plugin executor routing is unavailable while CLIProxyAPIHome is enabled.

## Build and install

```bash
make test
make install                     # copies to ~/.cli-proxy-api/plugins/<goos>/<goarch>/
make install PLUGINS_DIR=/path   # or somewhere else
```

Then merge [`config.example.yaml`](config.example.yaml) into your CLIProxyAPI config and restart it:

```bash
brew services restart cliproxyapi
```

The artifact file name must be `web-search-redirect.<dylib|so|dll>`, because the file name is the plugin ID.
Changes to the plugin's settings hot-reload with the config file. Replacing the `.dylib` requires a restart.

## Configuration

All keys live under `plugins.configs.web-search-redirect`. `enabled` and `priority` are host-level keys.

| Key | Default | Description |
|---|---|---|
| `models` | `[]` | Case-insensitive wildcard patterns (`*` only) of client models to redirect. An empty list matches nothing. List only models whose provider lacks hosted search; OpenAI, Codex, and xAI models already search natively. |
| `search_model` | `gpt-5.5` | Model that runs each search with the hosted `web_search` tool. |
| `search_provider` | `codex` | Built-in provider forced for search requests. Empty lets the host resolve `search_model`. |
| `search_reasoning_effort` | `low` | `reasoning.effort` for search requests. Empty keeps the provider default. |
| `max_searches` | `5` | Maximum searches per client request. Later calls get a "limit reached" reply so the model answers with what it has. |
| `debug_log` | `false` | Log each redirect and search through the host logger. |

## Using it with Codex

Codex sends the hosted `web_search` tool for any model when `web_search` is not disabled in its config.
The default is cached mode (`external_web_access: false`), which is forwarded to the search model as-is. For
live results, set this in `~/.codex/config.toml`:

```toml
web_search = "live"
```

## Notes

- Each search is a separate request on `search_model`, recorded in usage under that model.
  The OSS model's turns are recorded under the OSS model.
- Search failures don't fail the response. The model is told that the search failed, and the
  `web_search_call` item has status `failed`.
- Only the Responses API (`/v1/responses`) is handled. For Claude Code on the Messages API, see the
  `claude-web-search-router` example in the CLIProxyAPI repository.
