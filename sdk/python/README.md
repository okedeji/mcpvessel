# agentcage Python SDK

Convenience layer for building agentcage agents in Python. Every helper
here has a standard-library equivalent so authors who want full control
can opt out without changing what the platform does.

## Install

```bash
pip install agentcage-sdk
```

For local development against this checkout:

```bash
pip install -e ./sdk/python
```

## Quick start

```python
import agentcage

agent = agentcage.Agent("hello", "A trivial agent.")

@agent.tool()
def greet(name: str) -> str:
    """Greet someone warmly."""
    return agentcage.llm.complete(
        system="You are friendly and concise.",
        user=f"Greet {name} in one sentence.",
    )

if __name__ == "__main__":
    agent.run()
```

Pair with an `Agentfile`:

```
FROM python:3.12-slim
RUN pip install --no-cache-dir agentcage-sdk
MODEL anthropic/claude-3-5-sonnet
SECRETS anthropic_api_key
NETWORK allow:api.anthropic.com
META description "A trivial agent that greets warmly using the LLM."
ENTRYPOINT python3 agent.py
```

## Public surface

| Surface | Purpose |
|---|---|
| `agentcage.Agent` | The MCP server. Re-exports `mcp.server.fastmcp.FastMCP`. |
| `agentcage.llm.complete` | Single-turn LLM call. Routes to Anthropic or OpenAI based on the model name (`anthropic/claude-3-5-sonnet`, `openai/gpt-4o`). |
| `agentcage.llm.anthropic_client()` | Preconfigured Anthropic SDK client. Reach for this when you need multi-turn, tool use, streaming, vision, or anything else the Anthropic API exposes. |
| `agentcage.llm.openai_client()` | Preconfigured OpenAI SDK client. Same shape as above. |
| `agentcage.agents.<name>.<tool>` | Call a sub-agent declared in your `USES`. |
| `agentcage.run.id` | The current run's ID, useful for logging and trace correlation. |
| `agentcage.run.agent_ref` | The ref this run is executing (e.g. `@org/name:1.0`). |
| `agentcage.budget.total` | Tokens declared in the Agentfile's `BUDGET`. |
| `agentcage.budget.used` | Tokens this run has consumed so far. |
| `agentcage.budget.remaining_tokens()` | What's left. Clamps at 0. |

## Opting out

Every helper has a standard-library equivalent. If you don't want the
SDK, none of the platform semantics change.

| Instead of | Use |
|---|---|
| `agentcage.Agent` | `from mcp.server.fastmcp import FastMCP` |
| `agentcage.llm.complete(...)` | `anthropic.Anthropic().messages.create(...)` or `openai.OpenAI().chat.completions.create(...)` |
| `agentcage.llm.anthropic_client()` / `openai_client()` | Construct the native SDK client directly (handle the env var yourself) |
| `agentcage.agents.web_search.search(...)` | Construct an `mcp.ClientSession` against `os.environ["AGENTCAGE_USES_WEB_SEARCH_URL"]` |
| `agentcage.run.*` | Read `AGENTCAGE_RUN_ID`, `AGENTCAGE_AGENT_REF` directly |
| `agentcage.budget.*` | Read `AGENTCAGE_BUDGET`, `AGENTCAGE_BUDGET_USED` directly |

Pick whichever feels right. The platform doesn't care which surface you
build against.

## Development

```bash
pip install -e ./sdk/python[dev]
pytest sdk/python/tests/
```
