# Example catalog

A priced catalog to start a gateway of your own from: 94 Models over
seven Providers, each a manifest the gateway applies as it is.

| Directory | Holds |
|---|---|
| `providers/` | one Provider per vendor, with its public API address and the variable its key is read from |
| `models/` | one Model per file, named `<provider>__<model>.yaml`: one target, no fallback, and a price in USD per 1,000,000 tokens |

## The prices are a snapshot

The prices were taken on 2026-09-16. Vendors change them, and keeping
these files current is your installation's job: the gateway charges what
a Model's `spec.pricing` says and fetches no price from anywhere. Edit a
file and start the gateway again, and that one Model is updated.

## The windows and input modalities are a snapshot too

63 of the Models also carry `spec.contextWindow`, `spec.maxOutputTokens`
and `spec.modalities.input`, taken on 2026-09-27 from OpenRouter's
public model list (`https://openrouter.ai/api/v1/models`): its
`context_length`, `top_provider.max_completion_tokens` and
`architecture.input_modalities`. A Model is matched by its target's
upstream name under the vendor's OpenRouter prefix (`openai`,
`anthropic`, `google` for `gemini`, `moonshotai` for `moonshot`, `x-ai`
for `xai`, `z-ai` for `zhipu`), with an Anthropic version written with a
dot where the vendor writes a hyphen (`claude-opus-4-1` is
`anthropic/claude-opus-4.1`); a Model of the `openrouter` Provider is
matched by its target as it stands. The other 31 have no entry of that
name and carry no figure, which the gateway reads as unknown, never as
zero.

`maxOutputTokens` is the model's output limit and a ceiling, not a
default: a request translated to an `anthropic` target that names no
`max_tokens` is sent 4096, or the Model's `maxOutputTokens` when that is
lower.

## Using it

1. Export the key of each vendor you hold:

   | Provider | Dialect | Base URL | Variable | Models |
   |---|---|---|---|---|
   | `openai` | `openai` | `https://api.openai.com/v1` | `OPENAI_API_KEY` | 30 |
   | `anthropic` | `anthropic` | `https://api.anthropic.com/v1` | `ANTHROPIC_API_KEY` | 21 |
   | `zhipu` | `openai` | `https://api.z.ai/api/paas/v4` | `ZHIPU_API_KEY` | 17 |
   | `gemini` | `gemini` | `https://generativelanguage.googleapis.com/v1beta` | `GEMINI_API_KEY` | 11 |
   | `moonshot` | `openai` | `https://api.moonshot.ai/v1` | `MOONSHOT_API_KEY` | 8 |
   | `xai` | `openai` | `https://api.x.ai/v1` | `XAI_API_KEY` | 4 |
   | `openrouter` | `openai` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` | 3 |

2. Delete the vendors you do not hold, the Provider and its Models
   together, because a Provider whose variable is unset, or a Model
   naming a Provider that is not there, stops the gateway at start:

   ```sh
   rm deploy/catalog/providers/xai.yaml deploy/catalog/models/xai__*.yaml
   ```

3. Start `luxd` with `LUX_BOOTSTRAP_DIR=deploy/catalog`. The catalog is
   applied once at start and the control plane stays writable, so Keys
   and Budgets are created through `/v1` as usual. The whole first start
   is walked in [`docs/install.md`](../../docs/install.md).

Each Provider sets `discovery.mode: none`, so the gateway serves exactly
the Models listed here. Set it to `auto` to also list the vendor's other
models, which arrive without a price. A Model routes to its one target
and does not fall back; to fall back across vendors, give a Model more
targets. The Gemini Models answer through the `/gemini/v1beta` door.

The `openrouter` Provider sets no data policy for most requests, so
OpenRouter routes each under its account defaults. A request on a Key
with `spec.zeroRetention` is the exception: the Provider declares
`spec.zeroRetention` with `provider: {zdr: true}`, so such a request
reaches OpenRouter only with that option, held to endpoints with zero
data retention, as
[`docs/security.md`](../../docs/security.md#keep-a-workloads-requests-from-upstreams-that-may-keep-them)
explains. The other Providers declare nothing, so a zero-retention Key
reaches none of their Models until you add a declaration that matches
the vendor's terms. To hold every request through OpenRouter to those
endpoints, add `spec.requestFields` with `provider: {zdr: true}`, as
[`docs/security.md`](../../docs/security.md#hold-an-upstream-to-its-data-terms)
shows; the gateway then sets that member on every request, whatever a
caller sends. `data_collection: deny`, which leaves out the providers
that may train on the data, is a separate member that can sit beside
it.
