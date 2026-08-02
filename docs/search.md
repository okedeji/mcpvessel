# search

Find agents to pull or wrap. `search` queries the public MCP Registry by name and prints one row per matching server: its reverse-DNS name, latest version, eval signal, and description. Take a hit further with `mcpvessel pull <name>` to fetch the bundle, or `mcpvessel import <name>` to cage and build it. With `--local` it searches the bundles already in your store instead, so the same command finds a server whether it is published or sitting on your disk.

```
mcpvessel search QUERY [flags]
```

`QUERY` is required (exactly one argument). Registry search matches it against server names; local search matches it as a plain substring of each stored reference.

## Registry search (default)

Without `--local`, `search` calls the official MCP Registry at `https://registry.modelcontextprotocol.io`, `GET /v0.1/servers?search=<QUERY>&limit=<limit>`, and prints the page it gets back. The registry holds metadata only, never an agent's bytes: each entry points at the OCI artifact it was built from, and `search` moves nothing but the listing. Results come back newest first.

Only the first page is returned. `search` does not follow the registry's pagination cursor, so `--limit` is the effective ceiling on how many rows you see (default 20). A single call is bounded by a 30 second timeout; a wedged registry fails the command with an error rather than hanging.

An empty `QUERY` (passing `""`) lists the catalog instead of filtering, still capped at `--limit` and still just the first page.

### What a result row shows

The table has five columns, tab-aligned:

| Column | Source | Empty shows |
| --- | --- | --- |
| `NAME` | the entry's reverse-DNS name (`io.github.owner/server`) | always present |
| `VERSION` | the entry's current version | `-` |
| `SOURCE` | where the implementation comes from: `npm`, `pypi`, `oci`, or `remote` | `-` |
| `EVALS` | the eval signal the author stamped at publish, if any | `-` |
| `DESCRIPTION` | the server's description, clipped to 60 characters with a trailing `…` | blank |

### SOURCE, and why it decides everything

`SOURCE` is the column to read first, because it says whether the entry can be caged at all.

An `npm`, `pypi`, or `oci` entry ships code. mcpvessel can pull that package, put it in a container with no network beyond what you allow, and hold your secrets outside where the server cannot reach them. An entry publishing to several ecosystems shows them joined, `npm+pypi`.

A `remote` entry is a hosted URL that the publisher runs. There is no code on your machine, so there is nothing for a cage to contain, and [import](import.md) refuses it. That refusal is not a limitation to work around: the exposure with a remote server is whatever your client sends to someone else's servers, and no container on your machine can change that. Roughly half the public registry is remote-only, so this is a common result, not an edge case.

When any row is remote, `search` prints a count under the table so the shape of the result set is obvious at a glance.

Each row is one server at its current version. The registry stores a separate record per published version; `search` collapses them, so a server with a long release history takes one row rather than filling your `--limit` with its own back catalogue.

`EVALS` is only set when an author ran the bundle's eval suite before publishing and the signal survived the registry round-trip. It renders compactly:

- `47/50 j0.83`: 47 of 50 attempted checks passed, judge score 0.83. The left number is passes, the right is passes plus failures (total attempted). `j` and its score appear only when a judge score is present.
- `declared`: the author declared evals but no pass/fail counts came through.
- `-`: no eval signal, or the author never declared one.

The `NAME` you see is exactly what you feed to `pull` and `import`.

## Local search (--local)

With `--local`, `search` never touches the network. It lists every bundle in your local store and keeps the ones whose reference contains `QUERY` as a substring. The match is case-sensitive and literal, not a fuzzy or name-only search: `mcpvessel search github --local` finds `@me/github:0.1` because `github` appears in the reference.

The table is the store's own three columns:

| Column | Meaning |
| --- | --- |
| `REFERENCE` | the tag pointing at the bundle, or `<untagged>` for a content-addressed bundle no tag names |
| `HASH` | the bundle's files hash, shortened to 12 hex characters |
| `SIZE` | the `.agent` file size, human-readable |

Because the filter matches on the reference string, an untagged bundle (empty reference) is only returned when `QUERY` is `""`, which then lists every bundle in the store. `--limit` does not apply to `--local`; it is a registry-only cap and is ignored here.

## JSON output (--json)

`--json` replaces the table with indented JSON on stdout, for scripts and pipelines.

- Registry: an array of server records in the registry's `server.json` v0.1 shape (`name`, `description`, `version`, `packages`, `_meta`, and the rest), each with two fields mcpvessel adds: `source` (`npm`, `pypi`, `oci`, `npm+pypi`, or `remote`) and `cageable` (a boolean). Read those to decide whether an entry can be caged. Do **not** read `repository.source`, which is part of the registry's own record and says where the code is hosted (`github`), a different question with a confusingly similar name. The eval stamp rides inside `_meta`, under the publisher-provided slot, not as a top-level `evals` field, so a consumer reads it there rather than from a rendered `EVALS` string.
- Local: an array of store entries, each with `Ref`, `Hash`, and `Size`.

No results is an empty JSON array in both modes, and an empty table (header only) without `--json`.

## Flags

| Flag | Meaning |
| --- | --- |
| `--json` | Emit machine-readable JSON instead of the table. Applies to both modes. |
| `--local` | Search the local store by reference substring instead of the MCP Registry. Skips the network; ignores `--limit`. |
| `--limit N` | Maximum results from the registry. Default 20. `0` sends no limit and lets the registry decide the page size. Registry mode only. |

## Examples

```sh
# Search the registry for web-search servers.
mcpvessel search "web search"

# Narrow a common term and cap the page.
mcpvessel search filesystem --limit 5

# See what you already have locally that mentions fs.
mcpvessel search fs --local

# Machine-readable, to pipe a name into pull.
mcpvessel search github --json
```

## Notes

- `--limit 0` does not mean "no results". It omits the limit parameter, so the registry returns its own default page size.
- Registry search is name-oriented; it will not find a server by a word that appears only in its description. Local search matches the reference string only, not the description.
- A description longer than 60 characters is clipped for display with a trailing `…`. The full text is in `--json`.
- Local `--limit` is silently ignored rather than an error, so a script that always passes `--limit` still works with `--local`.
- Point at a non-official registry with `VESSEL_MCP_REGISTRY=<base-url>`; `search` uses it for the registry mode. `--local` is unaffected.
- `search` reads only. It resolves nothing, pulls nothing, and changes no state on disk or on the daemon.

## See also

- [import](import.md): cage and build a server you found, by the `NAME` a registry hit shows.
- [README: Ship it](../README.md#ship-it): publishing a bundle to the registry, which is what puts it in these results.
- [README: Commands](../README.md#commands): the full command list.
