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

`maxOutputTokens` is also the `max_tokens` a request translated to an
`anthropic` target is sent with when the caller names none, in place of
4096, so a Claude Model here allows its whole output on such a request.
Lower it, or delete it, where the vendor's output rate limit is counted
against `max_tokens` and that matters to you.

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
