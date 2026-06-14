# The agent contract

An agentcage agent is an MCP server in a container. There is no agentcage SDK
and nothing agentcage-specific to import. The platform reads your Agentfile,
starts your agent, and speaks MCP to it. Everything an agent needs to
participate, serve itself to a parent, call its own sub-agents, reach the LLM,
is carried by a handful of environment variables plus MCP.

This page is the contract. Honor it and your agent runs the same whether it is
written in Python, TypeScript, Rust, or hand-rolled JSON-RPC.

## The environment variables

The runtime injects these. Your agent reads them; it never sets them.

| Variable | Set on | Meaning |
|---|---|---|
| `AGENTCAGE_SERVE_HTTP` | every sub-agent | A bind address (`:8000`). When set, serve MCP over streamable-HTTP on it instead of stdio. |
| `AGENTCAGE_USES_<NAME>_URL` | a parent, one per `USES` | The URL to reach that sub-agent. `<NAME>` is the dependency name uppercased with dashes turned to underscores: `USES @org/web-search` gives `AGENTCAGE_USES_WEB_SEARCH_URL`. |
| `AGENTCAGE_LLM_URL` | every reasoning agent | An OpenAI-compatible endpoint. Call it instead of a provider directly; agentcage holds the keys and meters the cost. One URL per agent, so the gateway knows whose call it is. |

A `USES_<NAME>_URL` points at the gateway, not at the sub-agent directly. The
gateway routes to the real sub-agent and enforces any `DENY`/`BAN` policy on
that edge. From your code it is just a URL you open an MCP client to.

## Serving (a sub-agent)

When `AGENTCAGE_SERVE_HTTP` is set, serve streamable-HTTP. Three details that
are easy to miss, because most MCP libraries default the wrong way for a
sandboxed agent behind a gateway:

1. **Serve at `/mcp`.** The gateway forwards an edge to `http://<you>:PORT/mcp`.
   FastMCP's streamable-HTTP transport already defaults to `/mcp`; if your
   server defaults elsewhere, point it there.
2. **Bind all interfaces.** The bind address is `:8000` (no host), so listen on
   `0.0.0.0`, not loopback. A sibling container reaches you by name over the
   run network; loopback would only answer yourself.
3. **Turn off DNS-rebinding host checks.** The MCP SDK validates the `Host`
   header by default and rejects anything not on an allowlist with `421
   Misdirected Request`. The gateway forwards with your container's hostname,
   which is not on that list. On a private per-run network the gateway is the
   host boundary, so the check is redundant. Disable it.

A minimal Python sub-agent (raw `mcp`, no agentcage code):

```python
import os
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings

mcp = FastMCP("echo")

@mcp.tool()
def shout(text: str) -> str:
    return text.upper()

if __name__ == "__main__":
    serve = os.environ.get("AGENTCAGE_SERVE_HTTP")
    if serve:
        host, _, port = serve.rpartition(":")
        mcp.settings.host = host or "0.0.0.0"
        mcp.settings.port = int(port)
        mcp.settings.transport_security = TransportSecuritySettings(
            enable_dns_rebinding_protection=False
        )
        mcp.run(transport="streamable-http")
    else:
        mcp.run()  # stdio when run on its own
```

## Calling (a parent)

To call a sub-agent, open an MCP client to its `AGENTCAGE_USES_<NAME>_URL` and
call a tool. Two details:

1. **Tools that call out must be async.** A server framework runs your tool
   inside its own event loop, so an outbound MCP call has to be awaited.
   Calling `asyncio.run()` from inside a running loop raises. Declare the tool
   `async` and `await` the call.
2. **Unwrap nested exception groups.** A gateway `DENY`/`BAN` comes back as a
   clean `McpError` (for example `tool whisper denied by the gateway`). The
   Python streamable-HTTP client nests two anyio task groups, so that error
   arrives wrapped in two `ExceptionGroup`s. Unwrap to the innermost exception
   or your error message reads `unhandled errors in a TaskGroup` instead of the
   real reason.

```python
import os
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

def _root_cause(err: BaseException) -> str:
    while isinstance(err, BaseExceptionGroup) and err.exceptions:
        err = err.exceptions[0]
    return str(err)

async def call_sub(env_var: str, tool: str, args: dict) -> str:
    url = os.environ[env_var]   # e.g. AGENTCAGE_USES_ECHO_URL
    try:
        async with streamable_http_client(url) as (read, write, _):
            async with ClientSession(read, write) as session:
                await session.initialize()
                result = await session.call_tool(tool, args)
                return result.content[0].text
    except BaseException as err:
        raise RuntimeError(_root_cause(err)) from None
```

## Reaching the LLM

A reasoning agent calls the model through `AGENTCAGE_LLM_URL` with any
OpenAI-compatible client. agentcage holds the provider key, so your code never
sees one; the gateway routes the call to the operator's configured endpoint,
meters the cost, and debits the run's budget. You send a model name, but the
gateway decides which model actually runs (the operator can pin one, or fall
back when your provider is not configured), so treat your `MODEL` as advisory.

```python
import os
from openai import OpenAI

client = OpenAI(base_url=os.environ["AGENTCAGE_LLM_URL"], api_key="unused")
resp = client.chat.completions.create(
    model="gpt-4o",  # advisory; the gateway routes by the operator's config
    messages=[{"role": "user", "content": "..."}],
)
```

The `api_key` is ignored: the gateway attaches the real one. Speaking the
OpenAI completions surface is the requirement for using the gateway. An agent
that needs a provider or protocol agentcage does not proxy opts out with
bring-your-own: declare `SECRETS my_key` and `EGRESS allow:host`, and call that
provider directly. Its spend is outside the run's budget.

There is no `PROMPT` directive. If you want the operator to tune your system
prompt, read it from an `ENV` input (run-scoped) or take it as a tool argument
(per call), the same as any other application config.

## Own your logging

agentcage forwards your container's stderr verbatim. It does not filter or
quiet it; what you log is what the operator sees. Most MCP and HTTP libraries
log every request at INFO by default, which is noisy in a multi-agent run.
That output is your dependency's, not the platform's, so quiet it where you
configure logging:

```python
import logging
logging.getLogger("mcp").setLevel(logging.WARNING)
logging.getLogger("httpx").setLevel(logging.WARNING)
```

## The MAIN tool's shape

The tool an Agentfile declares as `MAIN` is what `agentcage run BUNDLE "..."`
routes the prompt to. By convention it takes `messages: list[dict]`, the same
`{role, content}` shape OpenAI and Anthropic accept; the CLI wraps a positional
prompt as one user turn. A tool reached with `agentcage call BUNDLE TOOL --arg
key=value` instead receives the arguments you pass. Either way the platform
routes the call and prints whatever the tool returns. A tool may return a
single value or stream its response; the gateway preserves streaming end to
end.

## Summary: the sharp edges

Everything above, in one list. Most agents only need a subset.

- Serve at `:PORT/mcp` from `AGENTCAGE_SERVE_HTTP`, bound on `0.0.0.0`.
- Disable the MCP SDK's DNS-rebinding host check when serving.
- Make tools that call sub-agents `async`.
- Unwrap nested `ExceptionGroup`s to read gateway `DENY`/`BAN` errors.
- Quiet your own libraries' logging; agentcage forwards stderr as-is.
- Reach sub-agents at `AGENTCAGE_USES_<NAME>_URL`, the LLM at `AGENTCAGE_LLM_URL`.
