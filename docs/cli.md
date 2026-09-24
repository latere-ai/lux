# The lux command

What `lux -help` prints, command by command. A test holds this file
equal to the binary's own output, so what is written here is what the
command says.

## lux

```
Usage: lux <command> [flags]

The lux command speaks the gateway's /v1 API from a shell or from an
agent: apply a manifest of any kind, read one object, list a kind,
delete, rotate a Key, read usage, ask a door which models a Key may
call, and say who the token belongs to. Output is the server's own
JSON unless -o asks for yaml, table, or wide. Exit 0 means done, 1
means the server refused or could not be reached, and 2 means the
command was wrong and nothing was sent.

Environment:
  LUX_URL         the gateway's URL (-url)
  LUX_TOKEN       a token from your issuer, for the /v1 commands (-token)
  LUX_TOKEN_FILE  a file holding the token, read on every request (-token-file)
  LUX_KEY         a Key value, for lux models (-key)

LUX_TOKEN opens /v1 and LUX_KEY opens a door; neither works on the other
side. A door command also reads LUX_BASE_URL and LUX_API_KEY, an SDK's
own two variables, when the four above are unset.

Commands:
  apply             apply manifests from files
  get               read one object
  list              list the objects of a kind
  delete            delete one object
  keys rotate       give a Key a new value
  usage             read usage aggregates
  requests          read request records
  models            list the models a Key may call, through the door
  serve             attach a local model runtime as a Provider
  whoami            say who the token belongs to
  keys create       create a Key from flags
  providers create  create a Provider from flags
  models create     create a Model from flags
  budgets create    create a Budget from flags

Flags:
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
  -version
    	print the build identity and exit
```

## lux apply

```
Usage: lux apply -f <file>... [flags]

Apply manifests from files. The kind and the name come from each
document, and a file may hold several YAML documents; they apply in
file order and the first refusal stops the run. A create of a Key
prints its value once, in status.value.

Flags:
  -credential-from-env value
    	NAME or <provider>=NAME: send the variable's value as the Provider's credential
  -f value
    	a manifest file; repeatable, applied in order
  -if-match string
    	apply only at this version, or * to update an existing object; one document
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux get

```
Usage: lux get <kind> <name> [flags]

Read one object with its status. <kind> is provider, model, key, or
budget, singular or plural; <name> is a name or an id.

Flags:
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux list

```
Usage: lux list <kind> [flags]

List the objects of a kind, following every page to the end or to
-limit items, and print one list. -source and -provider apply to
models alone.

Flags:
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -l value
    	a label selector, k=v; repeatable, every pair must match
  -limit int
    	stop after this many items; 0 is every item
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -owner string
    	objects of this owner
  -provider string
    	models alone: those with a target on this provider
  -source string
    	models alone: declared or discovered
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux delete

```
Usage: lux delete <kind> <name> [flags]

Delete one object. Nothing is printed on success.

Flags:
  -if-match string
    	delete only at this version, or * for any
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux keys rotate

```
Usage: lux keys rotate <name> [flags]

Give a Key a new value, keeping its name, id, limits, and budget. The
new value is printed once, in status.value.

Flags:
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux usage

```
Usage: lux usage [flags]

Read usage aggregated over a range, grouped by up to three dimensions
with -by and bucketed with -interval. -since 24h is -from now-24h.

Flags:
  -by value
    	a dimension to group by: key, model, provider, owner, door, status, or label:<k>; repeatable, at most three
  -from string
    	the start of the range, RFC 3339
  -interval string
    	the bucket: hour, day, month, or total
  -key value
    	a Key, by name or id; repeatable
  -label value
    	a Key label, k=v; repeatable
  -model value
    	a Model, by name or id; repeatable
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -owner value
    	an owner; repeatable
  -provider value
    	a Provider, by name or id; repeatable
  -since string
    	a duration back from now, such as 24h; the same as -from now-24h
  -to string
    	the end of the range, RFC 3339
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux requests

```
Usage: lux requests [flags]

Read the request records themselves, newest first, following every
page to the end or to -limit records.

Flags:
  -error string
    	records refused or failed with this code
  -from string
    	the start of the range, RFC 3339
  -key value
    	a Key, by name or id; repeatable
  -label value
    	a Key label, k=v; repeatable
  -limit int
    	stop after this many records; 0 is every record
  -model value
    	a Model, by name or id; repeatable
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -owner value
    	an owner; repeatable
  -provider value
    	a Provider, by name or id; repeatable
  -since string
    	a duration back from now, such as 24h; the same as -from now-24h
  -status string
    	ok, refused, or failed
  -to string
    	the end of the range, RFC 3339
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux models

```
Usage: lux models [flags]

Ask the lux door which models the Key in LUX_KEY may call right
now. This is the one command that sends a Key rather than a token.

Flags:
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux serve

```
Usage: lux serve -dialect <d> -upstream <u> -as <n> [flags]

Attach a model runtime on this machine to the gateway as a tunneled
Provider, which the gateway reaches over the session this command
holds open. The runtime's URL stays on this machine. The command runs
until it is stopped, connecting again when the session breaks, and
exits 1 when the gateway ends the session for a reason a retry cannot
fix.

Flags:
  -as string
    	the Provider to apply and attach; required
  -carriers int
    	streams parked at the gateway (default 4)
  -dialect string
    	the dialect the local runtime speaks: openai, anthropic, gemini, or lux; required
  -exclude value
    	a glob of model ids to leave out, written into the Provider; repeatable
  -include value
    	a glob of model ids to discover, written into the Provider; repeatable
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -label value
    	a label on the Provider, k=v; repeatable
  -no-apply
    	attach to an existing Provider instead of applying one
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -upstream string
    	the runtime's base URL on this machine, which stays on this machine; required
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux whoami

```
Usage: lux whoami [flags]

Say who the token belongs to: the subject, the claims, the policy, and
the limits this server holds for it.

Flags:
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux keys create

```
Usage: lux keys create <name> -models <m>[,<m>...] [flags]

Build a Key from flags and apply it, printing its value once. -dry-run
prints the manifest instead and sends nothing, which is how a first
file gets written.

Flags:
  -budget string
    	the Budget the Key draws from, or several, comma separated, each of which every call must fit
  -dry-run
    	print the manifest and send nothing
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -label value
    	a label, k=v; repeatable
  -models string
    	the model selectors, comma separated; required
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -passthrough
    	let the Key call any route the dialect has, translated or not
  -rpm int
    	requests a minute; 0 is no limit
  -spend string
    	a spend limit, amount[/window]
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -tpm int
    	tokens a minute; 0 is no limit
  -ttl string
    	how long the Key lives, a duration such as 720h
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux providers create

```
Usage: lux providers create <name> -dialect <d> -base-url <u> [flags]

Build a Provider from flags and apply it. The credential comes from
the environment variable -credential-from-env names and is never
written into the manifest -dry-run prints.

Flags:
  -base-url string
    	the provider's base URL; required
  -credential-from-env string
    	NAME: send the variable's value as the credential
  -dialect string
    	the dialect the provider speaks: openai, anthropic, gemini, or lux; required
  -dry-run
    	print the manifest and send nothing
  -header value
    	a header sent to the provider, k=v; repeatable
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux models create

```
Usage: lux models create <name> -target <provider>/<model>[@<weight>[:<priority>]]... [flags]

Build a Model from flags and apply it. -target repeats, one per
provider the model reaches.

Flags:
  -dry-run
    	print the manifest and send nothing
  -fallback string
    	onError or never
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -price-input string
    	the price of a million input tokens; with -price-output
  -price-output string
    	the price of a million output tokens; with -price-input
  -target value
    	a target, <provider>/<model>[@<weight>[:<priority>]]; repeatable, at least one
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
```

## lux budgets create

```
Usage: lux budgets create <name> -amount <a> [flags]

Build a Budget from flags and apply it. A budget is hard unless -soft
is given.

Flags:
  -amount string
    	the amount per window; required
  -anchor string
    	an RFC 3339 instant the window is aligned to, such as 2026-09-14T00:00:00Z for weeks from a Monday
  -currency string
    	the currency; USD when unset
  -dry-run
    	print the manifest and send nothing
  -key string
    	a Key value, for lux models; LUX_KEY when unset
  -o string
    	the output: json, yaml, table, or wide (default "json")
  -soft
    	warn past the amount instead of refusing
  -token string
    	a token from your issuer, for the /v1 commands; LUX_TOKEN when unset
  -token-file string
    	a file holding the token, read on every request; LUX_TOKEN_FILE when unset
  -url string
    	the gateway's URL; LUX_URL when unset
  -v	on a refusal, add the code, the paths, the detail, and the request id
  -window string
    	the window: month, none, or a duration; month when unset
```
