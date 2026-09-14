---
name: lux
description: "Give a program model access through a Lux gateway with the lux command: apply a manifest, create a Key under a Budget, read objects, and ask a door which models a Key may call."
---

# The lux command

`lux` speaks a Lux gateway's `/v1` API. Two variables reach it:

```sh
export LUX_URL=https://lux.example.com   # the gateway
export LUX_TOKEN=...                     # a token from your issuer
```

`LUX_TOKEN` opens `/v1` and `LUX_KEY` opens a door; neither works on
the other side. `LUX_TOKEN_FILE` names a file holding the token instead
of `LUX_TOKEN`, read on every request. Output is the server's JSON;
`-o yaml`, `-o table`, and `-o wide` render it locally.

## Exit codes

| Exit | Means | Do |
|---|---|---|
| 0 | done | read stdout |
| 1 | the server refused, failed, or could not be reached | read the sentence on stderr; rerun with `-v` for the code and the detail |
| 2 | the command was wrong and nothing was sent | fix the arguments or the variables |

A refusal prints one sentence on stderr:

```
$ lux apply -f provider.yaml
A field has a value it cannot take.
$ lux apply -f provider.yaml -v
A field has a value it cannot take.
code: invalid_field
paths: spec.baseURL
detail: a loopback host needs LUX_UPSTREAM_ALLOW_PRIVATE
request: req_01J9ZK2P7Q8R9S0T1U2V3W4X63
```

## Manifests

One file may hold several documents separated by `---`; they apply in
order and the first refusal stops the run. A credential never goes in a
file: `--credential-from-env NAME` sends the variable's value.

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openai
spec:
  dialect: openai
  baseURL: https://api.example.com/v1
---
apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: gpt-5
spec:
  targets:
    - provider: openai
      model: gpt-5
  pricing:
    input: "1.25"
    output: "10"
---
apiVersion: lux.latere.ai/v1beta1
kind: Budget
metadata:
  name: research
spec:
  amount: "50"
  window: month
---
apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: research-agent
spec:
  models: [gpt-5]
  budget: research
  ttl: 720h
```

```sh
lux apply -f stack.yaml --credential-from-env OPENAI_API_KEY
```

A Key's value is in `status.value` of the body a create prints, once.
Store it; a later `lux get` never carries it, and `lux keys rotate
<name>` mints a new one.

## Without a file

```sh
lux budgets create research --amount 50 --window month
lux keys create research-agent --models gpt-5 --budget research --ttl 720h
lux keys create research-agent --models gpt-5 --dry-run   # prints the manifest, sends nothing
```

## Reading

```sh
lux get key research-agent
lux get key research-agent -o yaml
lux list keys -o table
lux list models --source discovered --provider openai
lux whoami
lux usage --since 24h --by model
lux requests --status refused --limit 20
lux delete key research-agent
```

`<kind>` is `provider`, `model`, `key`, or `budget`, singular or
plural. `lux list` follows every page and prints one list; `--limit n`
stops after `n` items.

## Calling a model

A door is an SDK base URL and the Key is its API key:
`$LUX_URL/openai` for an OpenAI SDK, `$LUX_URL/anthropic` for an
Anthropic SDK, `$LUX_URL/gemini`, and `$LUX_URL/lux`. Ask a door what
the Key may call:

```sh
export LUX_KEY=lux_...                   # the value the create printed
lux models
```

The answer is the OpenAI list shape, `{"object":"list","data":[...]}`,
with one entry per model this Key may call now.

## Reading a refusal

| Code | Next step |
|---|---|
| `unauthenticated` | the token or the Key is wrong or expired; get a fresh one |
| `forbidden` | you may not do this; ask whoever administers the gateway |
| `not_found` | the name or a name the manifest refers to does not exist |
| `invalid_field`, `missing_field`, `unknown_field` | fix the field `-v` names under `paths` |
| `already_exists` | the name is taken; pick another or `lux get` it |
| `conflict` | the object changed since you read it; `lux get` it and apply again |
| `rate_limited`, `spend_exceeded`, `budget_exhausted` | wait the seconds the extra line names |
| `model_not_allowed` | the Key's `spec.models` does not match the model |

Every other code is exit 1 with its own sentence; `-v` adds the detail.
