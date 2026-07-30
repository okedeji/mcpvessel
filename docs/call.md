# call

Call a named tool on an agent by name, instead of routing a prompt to its `MAIN` the way `mcpvessel run` does. `call` is how you reach a tool collection (a wrapped server with no `MAIN`), or hit one specific tool on an agent directly. It boots the bundle in a cage through the daemon, makes one tool call with the arguments you pass, prints the result, and tears the cage down.

```
mcpvessel call BUNDLE TOOL [--arg KEY=VALUE ...]
```

`call` takes exactly two positional arguments: the bundle and the tool name. Everything else the tool needs comes through repeated `--arg` flags.

## What a BUNDLE can be

The first argument resolves the same four ways `run` and `serve` accept, through `resolveLocalTarget`:

- **A source directory** (one holding a `Vesselfile`). `call` builds it into your store first, with build progress on your terminal, then boots the resulting content hash. An unchanged directory is a cheap store hit, not a rebuild, so `call ./dir` behaves like `serve ./dir`. A directory with no `Vesselfile` is rejected with a message telling you to point at an agent source directory, a reference, a content hash, or a `.agent` file.
- **A reference** like `@okedeji/web-search:0.1`. Resolved store-first, then pulled from the registry only when your store does not hold it.
- **A content hash**, the addressable id an untagged build printed.
- **A path to a `.agent` file**, a built bundle on disk.

Whatever the form, `call` reads the bundle's `manifest.json` to check the tool is public and to find its input schema, then hands the resolved reference to the daemon to boot.

## Which tools you can call

A tool is callable only if the bundle makes it public. `assertToolIsPublic` enforces this before the cage starts, so a call to a private tool fails fast without booting anything.

- **`MAIN` is implicitly public.** The tool the Vesselfile declares as `MAIN` is always callable by name (though for an agent with a `MAIN` you would normally use `run`).
- **`EXPOSE`d tools are public.** Any tool named in the Vesselfile's `EXPOSE` list is callable.
- **Everything else is private.** A tool the agent serves over MCP but does not `EXPOSE` cannot be called. The error names the tool you asked for and lists the public tools you can call instead. If the bundle has no public tools at all, the error says so plainly.

The manifest's tool catalog is the authoritative gate. A normal build introspects the running server and records every tool with its resolved visibility (`main`, `public`, or `private`), including `EXPOSE *` already expanded to concrete names. When that catalog is present `call` gates on it directly. A bundle built without introspection, with no named `MAIN` or `EXPOSE`, carries no catalog; `call` then falls back to the raw Vesselfile directives, where `MAIN` and each `EXPOSE` name are public and a bare `EXPOSE *` admits any tool name (the wildcard expands only at introspection, so with no catalog the name passes the gate and the cage validates it).

## Passing arguments

Each `--arg KEY=VALUE` becomes one entry in the tool's argument map. The flag is repeatable, once per argument. `parseArgPairs` splits on the first `=`, so `--arg query=a=b` yields the key `query` with the value `a=b`. The key is trimmed of surrounding whitespace; the value is taken verbatim, untrimmed. `--arg KEY=` passes an empty string. A pair with no `=`, or with an empty key, is rejected. Repeat a key and the last one wins.

Values arrive as strings, but MCP tools expect typed JSON. `call` coerces each value to the type the tool's input schema declares for that key (`coerceArg` reading `propType`). A property typed as a union like `["string", "null"]` coerces to its first non-null member.

| Declared type | How the value is coerced |
| --- | --- |
| `string` | Kept verbatim, even when it looks like JSON. `--arg q="[1,2]"` stays the literal string. |
| `integer` | Parsed as a base-10 64-bit int. |
| `number` | Parsed as a 64-bit float. |
| `boolean` | Parsed by Go's rules: `1`, `t`, `true`, `0`, `f`, `false`, and their case variants. |
| `array`, `object` | Parsed as JSON. `--arg items='["a","b"]'`, `--arg opts='{"deep":true}'`. |
| none, or key not in the schema | Parsed as JSON if it parses, otherwise kept as a string. So `--arg n=5` becomes the number 5, `--arg s=hi` stays `"hi"`. |

Coercion is best effort. A value that does not parse to its declared type falls back to the raw string, and the server inside the cage validates it and reports a clear error. That fallback also covers a required argument you leave out: `call` only sends the pairs you pass, and the server rejects a missing or malformed argument.

Type coercion needs a schema, and only an introspected build carries one. A bundle built without introspection has no per-tool schema, so every `--arg` on it takes the last row's JSON-or-string path.

## What it returns

`call` sends the request to the daemon over its Unix socket and streams the run. The agent's logs (its stderr) go to your terminal's stderr as the run proceeds. The tool's result goes to stdout, with a trailing newline added if the result lacks one, so stdout carries just the tool output and is clean to pipe or capture. The result is the first text block the tool returns. A tool that reports an error surfaces as a non-zero exit with the tool's error text.

`call` needs the daemon. If it cannot reach it, the error tells you to run `mcpvessel init` to start it.

## Flags

| Flag | Meaning |
| --- | --- |
| `--arg KEY=VALUE` | One tool argument. Repeatable, once per argument. Split on the first `=`; the key is trimmed, the value is verbatim. Coerced to the tool's declared type (see [Passing arguments](#passing-arguments)). |
| `--secret NAME` | Supply a secret the agent declares, or `agent:NAME` to grant one agent of several. Resolved from your environment or the secret store, never the command line. Repeatable. |
| `--secret-file PATH` | Read secret values (`[agent:]NAME=VALUE` per line) from a permissions-restricted file. |
| `--env KEY=VALUE` | Supply an env value, or `KEY` to pass it through from your environment. Repeatable. |
| `--env-file PATH` | Read env values (`KEY=VALUE` per line) from a file. |
| `--egress HOSTS` | Allow the agent hosts for this call: `host,host`, or `agent:host,host` to scope one of several. Repeatable. |
| `--egress-inspect` | Decrypt the cage's outbound HTTPS to an approved host to record what it sent, and capture a not-yet-approved request so you can read it before allowing the host. Opt-in; granted secrets are redacted in the preview. See [egress](egress.md#seeing-what-a-server-sends---egress-inspect). |
| `--budget USD` | Cap the call's LLM spend, e.g. `5.00`. Only matters when the tool belongs to a reasoning agent. |

These are the same input flags `run` takes, resolved the same way: flags overlay your config-bound secrets, and a server still only receives a name its `SECRETS` declares.

## Examples

```sh
# Call a search tool on a published tool collection, one typed argument.
mcpvessel call @okedeji/web-search:0.1 search --arg query="agentic memory"

# Call a tool on a built source directory (built first, like serve).
mcpvessel call ./researcher fetch_paper --arg doi=10.1234/x.2026

# Several arguments, mixing types the schema declares.
mcpvessel call @me/github:0.1 list_issues --arg repo=okedeji/mcpvessel --arg limit=20 --arg open=true

# Pass a JSON array or object for an array/object argument.
mcpvessel call @me/db:0.1 query --arg table=users --arg columns='["id","email"]'

# Capture just the result; logs go to stderr, so stdout stays clean.
mcpvessel call @okedeji/web-search:0.1 search --arg query="mcp" > result.txt
```

## Notes

- The public-tool check runs before the cage boots, so calling a private tool fails immediately and cheaply, without starting a container.
- A source directory is built into your store on first call and reused on later calls while it is unchanged. Editing the directory triggers a rebuild on the next call.
- `--arg` splits on the first `=` only, so values may contain `=`. The key is whitespace-trimmed; the value is not. `--arg k=` sends an empty string, and a repeated key keeps the last value.
- Type coercion depends on the tool's schema, which only an introspected build records. Without a schema, every `--arg` is parsed as JSON when it can be and left as a string otherwise, so an unquoted `5` or `true` becomes a number or boolean, not text.
- A string-typed argument is never reinterpreted, even when its value looks like JSON. Use it to pass a literal that would otherwise coerce.
- Coercion never blocks a call. A value that does not match its declared type is sent as the raw string and the caged server validates it, so the error you see is the tool's own.
- `call` prints the tool's first text result and nothing else on stdout. Agent logs and progress go to stderr.

## See also

- [run](run.md): route a prompt to a bundle's `MAIN` instead of calling a tool by name; the sibling command with the full flag set (secrets, env, budget, egress).
- [serve](serve.md): stand a bundle up on a URL and call its tools repeatedly over MCP or REST, rather than one boot per call.
- [import](import.md), [build](build.md): produce the bundle you call. A wrapped tool collection has no `MAIN`, so `call` is how you reach its tools.
- [VESSELFILE.md](VESSELFILE.md): the `MAIN` and `EXPOSE` directives that decide which tools `call` can reach.
- [Cage it](../README.md#cage-it): the end-to-end walkthrough of wrapping a server and calling its tools.
