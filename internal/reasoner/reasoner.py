"""mcpvessel reasoner: a reusable, production-minded reasoning harness.

It serves one MCP tool, `respond(messages)`, that answers a request by running
an LLM tool-use loop over whatever tools it reaches through its USES sub-agents.
It reads VESSEL_USES_*_URL (each a tool server behind the gateway) and
VESSEL_LLM_URL (an OpenAI-compatible endpoint that never exposes a provider
key), so it holds no key of its own and boots fine with zero USES tools.

This file is written into the generated reasoning agent, so it is yours to edit:
tune the loop, the prompt, or the model handling, or replace it entirely.
Nothing here is mcpvessel-specific beyond the environment contract in the
sibling README.

Streaming: when a caller sets an MCP progress token on the tool call (mcpvessel
`serve` does this for a REST `stream:true` request), the loop reports the answer
tokens as they generate via MCP progress notifications. With no token, the same
run just returns the finished answer, so `run`, `call`, and non-streaming
callers are unaffected. The final tool result is always the complete answer.
"""

import asyncio
import contextlib
import json
import logging
import os
import sys

from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client
from mcp.server.fastmcp import Context, FastMCP
from mcp.server.transport_security import TransportSecuritySettings
from openai import APIError, AsyncOpenAI

# mcpvessel forwards this container's stderr verbatim, so quiet the libraries'
# default request logging here rather than expecting the platform to.
logging.getLogger("mcp").setLevel(logging.WARNING)
logging.getLogger("httpx").setLevel(logging.WARNING)

# The provider/model is the operator's decision, resolved by the LLM gateway,
# which rewrites the model field on the wire; the value sent here is only a
# placeholder the gateway overrides. Operator knobs use non-reserved env names
# because the Vesselfile parser forbids ENV keys under the VESSEL_ prefix.
MODEL = "gpt-4o-mini"

# MAX_TURNS bounds a runaway tool loop; the per-run budget is the real ceiling,
# enforced by the gateway. MAX_RETRIES rides the OpenAI SDK's own backoff, which
# honors Retry-After, so a rate limit or a 5xx blip does not kill a deployed
# run. The two char caps keep one oversized tool result, or a long loop, from
# overflowing the model's context window mid-run.
MAX_TURNS = int(os.environ.get("REASONER_MAX_TURNS", "12"))
MAX_RETRIES = int(os.environ.get("REASONER_MAX_RETRIES", "5"))
LLM_TIMEOUT = float(os.environ.get("REASONER_LLM_TIMEOUT", "120"))
MAX_TOOL_CHARS = int(os.environ.get("REASONER_MAX_TOOL_CHARS", "16000"))
# A tool that fails this many times with identical arguments is cut off, so the
# model cannot burn the whole budget retrying a call that will never succeed.
MAX_TOOL_FAILURES = int(os.environ.get("REASONER_MAX_TOOL_FAILURES", "3"))

# The internal prompt is the robust scaffold every reasoning agent gets: the
# discipline that keeps a tool-using model honest. An operator's --prompt is
# appended as domain instructions, never replacing this, so a deployed agent
# always keeps the guardrails below.
BASE_SYSTEM_PROMPT = """You are an autonomous agent that answers a request by calling tools.

Operating rules:
- Use only the tools provided to you. Never claim to have used a tool you did not call, and never invent a tool's output.
- Ground every factual claim in a tool result or the user's own message. If you do not have a tool or the information to answer, say so plainly rather than guessing.
- When a tool returns an error, do not pretend it succeeded. Try a different approach or tool if one fits; otherwise report what failed and why.
- Call tools only when they move the task forward. Do not repeat a call with the same arguments hoping for a different result.
- Stop and give a final answer as soon as you have enough to answer well. Do not keep calling tools once you can respond.
- Be accurate and concise. Prefer a short, correct answer over a long, padded one."""


def _system_prompt() -> str:
    """The internal scaffold plus the operator's addendum, if any. The addendum
    comes from a file (REASONER_SYSTEM_PROMPT_FILE, so it can be multi-line and
    hold any characters) or, as a fallback, an inline env value."""
    addendum = ""
    path = os.environ.get("REASONER_SYSTEM_PROMPT_FILE")
    if path:
        try:
            with open(path, encoding="utf-8") as f:
                addendum = f.read().strip()
        except OSError as err:
            print(f"[reasoner] could not read system prompt file {path}: {err}", file=sys.stderr, flush=True)
    if not addendum:
        addendum = os.environ.get("REASONER_SYSTEM_PROMPT", "").strip()
    if addendum:
        return BASE_SYSTEM_PROMPT + "\n\nAdditional instructions for this agent:\n" + addendum
    return BASE_SYSTEM_PROMPT


mcp = FastMCP("reasoner")


def _root_cause(err: BaseException) -> str:
    # The streamable-HTTP client nests task groups, so a gateway DENY/BAN or a
    # tool error comes back buried inside ExceptionGroups. Unwrap to the message.
    while isinstance(err, BaseExceptionGroup) and err.exceptions:
        err = err.exceptions[0]
    return str(err)


def _uses_urls() -> dict[str, str]:
    """Each USES edge is injected as VESSEL_USES_<ALIAS>_URL. The alias labels a
    tool when two sub-agents serve tools of the same name."""
    import re

    out: dict[str, str] = {}
    for key, value in os.environ.items():
        match = re.fullmatch(r"VESSEL_USES_(.+)_URL", key)
        if match and value:
            out[match.group(1).lower()] = value
    return out


def _sanitize(name: str) -> str:
    # OpenAI function names allow [a-zA-Z0-9_-]; keep MCP tool names in range.
    import re

    return re.sub(r"[^a-zA-Z0-9_-]", "_", name)


def _truncate(text: str) -> str:
    # A tool result larger than the cap is trimmed with a visible marker, so a
    # single big payload cannot blow the context window and hard-fail the run.
    if len(text) <= MAX_TOOL_CHARS:
        return text
    head = text[:MAX_TOOL_CHARS]
    return head + f"\n\n[truncated {len(text) - MAX_TOOL_CHARS} characters of tool output]"


class _Tools:
    """Holds the sub-agent MCP sessions open for one respond() call, so each
    tool dispatch reuses a live connection instead of reconnecting. Sessions
    are opened once, listed once, and closed together when the call ends."""

    def __init__(self) -> None:
        self.schema: list[dict] = []
        self.routes: dict[str, tuple[str, str]] = {}
        self.failed: list[str] = []
        self._sessions: dict[str, ClientSession] = {}
        self._stack = contextlib.AsyncExitStack()

    async def open(self, urls: dict[str, str]) -> None:
        for alias, url in urls.items():
            try:
                read, write, _ = await self._stack.enter_async_context(streamable_http_client(url))
                session = await self._stack.enter_async_context(ClientSession(read, write))
                await session.initialize()
                listed = await session.list_tools()
            except BaseException as err:
                print(f"[reasoner] listing tools from {alias} failed: {_root_cause(err)}", file=sys.stderr, flush=True)
                self.failed.append(alias)
                continue
            self._sessions[alias] = session
            for tool in listed.tools:
                exposed = _sanitize(tool.name)
                if exposed in self.routes:
                    exposed = _sanitize(f"{alias}_{tool.name}")
                self.routes[exposed] = (alias, tool.name)
                self.schema.append(
                    {
                        "type": "function",
                        "function": {
                            "name": exposed,
                            "description": tool.description or "",
                            "parameters": tool.inputSchema or {"type": "object", "properties": {}},
                        },
                    }
                )

    async def dispatch(self, exposed: str, arguments: dict) -> str:
        alias, real = self.routes[exposed]
        session = self._sessions[alias]
        result = await session.call_tool(real, arguments)
        parts = [c.text for c in result.content if getattr(c, "text", None)]
        return "\n".join(parts) if parts else "(no output)"

    async def aclose(self) -> None:
        await self._stack.aclose()


async def _complete(client: AsyncOpenAI, kwargs: dict, ctx: Context, stream_answer: bool):
    """Run one streamed completion, reporting content tokens as progress when
    stream_answer is set, and return (content, tool_calls, finish_reason). The
    OpenAI SDK retries the initial request on 429/5xx/timeout with backoff that
    honors Retry-After; a mid-stream drop raises here and the caller decides."""
    content_parts: list[str] = []
    calls: dict[int, dict] = {}
    finish = None
    stream = await client.chat.completions.create(**kwargs, stream=True)
    async for chunk in stream:
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        if choice.finish_reason:
            finish = choice.finish_reason
        delta = choice.delta
        if delta.content:
            content_parts.append(delta.content)
            if stream_answer:
                await _report(ctx, delta.content)
        for tcd in delta.tool_calls or []:
            slot = calls.setdefault(tcd.index, {"id": None, "name": "", "arguments": ""})
            if tcd.id:
                slot["id"] = tcd.id
            if tcd.function and tcd.function.name:
                slot["name"] += tcd.function.name
            if tcd.function and tcd.function.arguments:
                slot["arguments"] += tcd.function.arguments
    tool_calls = [calls[i] for i in sorted(calls) if calls[i]["id"]]
    return "".join(content_parts), tool_calls, finish


# _progress counts up across a respond() call; the value is not meaningful on
# its own, but MCP progress notifications require a monotonic number.
_progress = {"n": 0.0}


async def _report(ctx: Context, text: str) -> None:
    # Best effort: FastMCP no-ops when the caller set no progress token (run,
    # call, non-streaming serve), so this streams only when someone asked. A
    # reporting failure never fails the run; the full answer still returns.
    if not text:
        return
    _progress["n"] += 1
    try:
        await ctx.report_progress(progress=_progress["n"], message=text)
    except Exception:
        pass


@mcp.tool()
async def respond(messages: list[dict] = None, ctx: Context = None) -> str:
    """Answer the user's request, using the available tools as needed."""
    conversation = [{"role": "system", "content": _system_prompt()}]
    conversation += messages or []

    tools = _Tools()
    await tools.open(_uses_urls())
    # Refuse if any declared tool server dropped out, rather than answer over a
    # partial toolset and let the model claim it used a tool it never reached.
    if tools.failed:
        await tools.aclose()
        return (
            "reasoning stopped: could not reach tool server(s): "
            + ", ".join(sorted(tools.failed))
            + ". Refusing rather than answer without a tool this agent declares; check that the sub-agents are healthy and retry."
        )

    client = AsyncOpenAI(base_url=os.environ["VESSEL_LLM_URL"], api_key="unused", timeout=LLM_TIMEOUT, max_retries=MAX_RETRIES)
    # Count identical tool failures so a doomed call is cut off, not retried
    # until the turn limit burns the budget.
    failures: dict[str, int] = {}

    try:
        for turn in range(MAX_TURNS):
            kwargs = {"model": MODEL, "messages": conversation}
            if tools.schema:
                kwargs["tools"] = tools.schema
                kwargs["tool_choice"] = "auto"
            try:
                content, tool_calls, _ = await _complete(client, kwargs, ctx, stream_answer=True)
            except APIError as err:
                return f"reasoning stopped: {_root_cause(err)}"
            except BaseException as err:
                return f"reasoning stopped: {_root_cause(err)}"

            if not tool_calls:
                return content

            conversation.append(
                {
                    "role": "assistant",
                    "content": content or "",
                    "tool_calls": [
                        {"id": tc["id"], "type": "function", "function": {"name": tc["name"], "arguments": tc["arguments"]}}
                        for tc in tool_calls
                    ],
                }
            )
            for tc in tool_calls:
                content_out = await _run_tool(tools, tc, failures)
                conversation.append({"role": "tool", "tool_call_id": tc["id"], "content": content_out})

        # Turn limit reached: make one final tool-free pass so the caller gets a
        # best-effort answer from what the loop already gathered, not an error.
        return await _finalize(client, conversation, ctx)
    finally:
        await tools.aclose()


async def _run_tool(tools: _Tools, tc: dict, failures: dict) -> str:
    """Dispatch one tool call, feeding a bad-arguments error or a repeated
    failure back to the model as the tool result so it can correct course."""
    name = tc["name"]
    raw = tc["arguments"] or "{}"
    try:
        arguments = json.loads(raw)
        if not isinstance(arguments, dict):
            raise ValueError("arguments must be a JSON object")
    except (json.JSONDecodeError, ValueError) as err:
        return f"error: could not parse arguments as JSON ({err}). Re-issue the call with valid JSON arguments."

    if name not in tools.routes:
        return f"error: unknown tool {name}"

    key = name + "\x00" + raw
    if failures.get(key, 0) >= MAX_TOOL_FAILURES:
        return f"error: {name} has failed {MAX_TOOL_FAILURES} times with these arguments; stop calling it and answer with what you have or explain the limitation."
    try:
        return _truncate(await tools.dispatch(name, arguments))
    except BaseException as err:
        failures[key] = failures.get(key, 0) + 1
        return f"error: {_root_cause(err)}"


async def _finalize(client: AsyncOpenAI, conversation: list, ctx: Context) -> str:
    """One last completion with no tools, forcing a text answer after the turn
    limit rather than returning a bare 'limit reached'."""
    conversation.append(
        {
            "role": "system",
            "content": "You have reached the tool-call limit. Give your best final answer now using what you already have, and note any part you could not complete.",
        }
    )
    try:
        content, _, _ = await _complete(client, {"model": MODEL, "messages": conversation}, ctx, stream_answer=True)
        return content or "reasoning stopped: reached the tool-call limit without a final answer"
    except BaseException as err:
        return f"reasoning stopped: reached the tool-call limit ({_root_cause(err)})"


if __name__ == "__main__":
    serve = os.environ.get("VESSEL_SERVE_HTTP")
    if serve:
        host, _, port = serve.rpartition(":")
        mcp.settings.host = host or "0.0.0.0"
        mcp.settings.port = int(port)
        # The gateway on a private per-run network is the trust boundary, so the
        # SDK's own DNS-rebinding host check (which 421s the forwarded Host) is
        # redundant here. Turn it off, matching the other sample agents.
        mcp.settings.transport_security = TransportSecuritySettings(enable_dns_rebinding_protection=False)
        mcp.run(transport="streamable-http")
    else:
        mcp.run()
