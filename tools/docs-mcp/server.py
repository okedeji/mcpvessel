"""mcpvessel docs assistant: answers questions about mcpvessel by searching its
docs, and its source when the docs fall short, live on GitHub.

Nothing is baked in. It fetches lazily on a tool call, never at startup, so
tools/list works with no network during introspection; the searches then need
egress to GitHub, which the operator allows with
`--egress api.github.com,raw.githubusercontent.com`. A GitHub token is optional
(the GITHUB_PERSONAL_ACCESS_TOKEN secret): docs search works without it, and a
token turns on code search.

Retrieval is BM25 over heading-aware chunks with light domain-synonym expansion
and exact-flag boosting. The calling model reads the returned chunks and writes
the answer, so the tool aims for recall (surface the right sections) and leaves
final synthesis to the brain."""

import json
import math
import os
import re
import urllib.parse
import urllib.request

from mcp.server.fastmcp import FastMCP

REPO = os.environ.get("MCPVESSEL_DOCS_REPO", "okedeji/mcpvessel")
BRANCH = os.environ.get("MCPVESSEL_DOCS_BRANCH", "main")

mcp = FastMCP("mcpvessel-docs")

# Map a user's word to the words the docs actually use, so "stop a server" finds
# the teardown docs and "key" finds "secret".
SYNONYMS = {
    "stop": ["teardown", "release", "kill"],
    "kill": ["teardown", "release", "stop"],
    "delete": ["remove", "rm"],
    "remove": ["delete"],
    "key": ["secret", "token", "credential"],
    "token": ["secret", "credential"],
    "credential": ["secret", "token"],
    "secret": ["token", "credential"],
    "network": ["egress", "outbound", "host"],
    "outbound": ["egress", "network"],
    "host": ["egress"],
    "egress": ["network", "outbound", "host"],
    "isolate": ["cage", "sandbox", "isolation"],
    "sandbox": ["cage", "isolate", "isolation"],
    "cage": ["sandbox", "isolate", "isolation"],
    "publish": ["push", "register", "ship", "distribute"],
    "share": ["push", "ship", "distribute"],
    "reason": ["reasoning", "agent", "brain"],
    "discover": ["observe", "audit"],
    "watch": ["observe", "audit"],
}

_index = None  # cached: {"chunks", "tokens", "df", "avgdl", "n"}


def _get(url, accept=None):
    req = urllib.request.Request(url)
    if accept:
        req.add_header("Accept", accept)
    token = os.environ.get("GITHUB_PERSONAL_ACCESS_TOKEN", "")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=20) as resp:
        return resp.read().decode("utf-8", "replace")


def _tokenize(text):
    return re.findall(r"[a-z0-9][a-z0-9_.-]*", text.lower())


def _is_doc(path):
    """A real doc: top-level markdown (README, SECURITY, ...) or anything under
    docs/. Skips issue templates, sub-package READMEs, and vendored markdown so
    doc search stays on the actual documentation."""
    return path.endswith(".md") and ("/" not in path or path.startswith("docs/"))


def _chunk(path, text):
    """Split markdown into sections, each carrying its heading breadcrumb (so a
    chunk knows it is under 'Cage it > serving several'). Lines inside ``` code
    fences are never treated as headings, so shell comments do not become junk
    sections."""
    chunks, stack, title, body, fenced = [], [], path, [], False

    def flush():
        joined = "\n".join(body).strip()
        if joined:
            crumb = " > ".join(t for _, t in stack) or path
            chunks.append({"source": path, "crumb": crumb, "title": title, "text": joined})

    for line in text.split("\n"):
        if line.lstrip().startswith("```"):
            fenced = not fenced
            body.append(line)
            continue
        m = None if fenced else re.match(r"^(#{1,6})\s+(.+)$", line)
        if m:
            flush()
            level = len(m.group(1))
            title = m.group(2).strip()
            while stack and stack[-1][0] >= level:
                stack.pop()
            stack.append((level, title))
            body = []
        else:
            body.append(line)
    flush()
    return chunks


def _load_index():
    global _index
    if _index is not None:
        return _index
    tree = json.loads(_get(f"https://api.github.com/repos/{REPO}/git/trees/{BRANCH}?recursive=1"))
    paths = [t["path"] for t in tree.get("tree", []) if t.get("type") == "blob" and _is_doc(t["path"])]
    chunks = []
    for path in paths:
        try:
            chunks.extend(_chunk(path, _get(f"https://raw.githubusercontent.com/{REPO}/{BRANCH}/{path}")))
        except Exception:
            continue
    tokens = [_tokenize(c["title"] + " " + c["text"]) for c in chunks]
    df = {}
    for toks in tokens:
        for t in set(toks):
            df[t] = df.get(t, 0) + 1
    n = max(len(chunks), 1)
    avgdl = sum(len(t) for t in tokens) / n
    _index = {"chunks": chunks, "tokens": tokens, "df": df, "avgdl": avgdl, "n": n}
    return _index


def _rank(idx, query, k=7):
    q_terms = _tokenize(query)
    expanded = list(q_terms)
    for t in q_terms:
        expanded.extend(SYNONYMS.get(t, []))
    # Literal flags/commands are near-certain signals for a CLI tool's docs.
    exact = re.findall(r"--[a-z][a-z-]*|mcpvessel\s+[a-z]+", query.lower())

    k1, b = 1.5, 0.75
    scored = []
    for i, c in enumerate(idx["chunks"]):
        toks = idx["tokens"][i]
        if not toks:
            continue
        tf = {}
        for t in toks:
            tf[t] = tf.get(t, 0) + 1
        title_toks = set(_tokenize(c["title"]))
        score = 0.0
        for t in set(expanded):
            f = tf.get(t, 0)
            if not f:
                continue
            df = idx["df"].get(t, 0)
            idf = math.log(1 + (idx["n"] - df + 0.5) / (df + 0.5))
            s = idf * (f * (k1 + 1)) / (f + k1 * (1 - b + b * len(toks) / idx["avgdl"]))
            if t in title_toks:  # a heading match is worth much more
                s *= 2.5
            score += s
        haystack = (c["title"] + " " + c["text"]).lower()
        for e in exact:
            if e.strip() and e.strip() in haystack:
                score += 6.0
        if score > 0:
            scored.append((score, c))
    scored.sort(key=lambda x: x[0], reverse=True)
    return [c for _, c in scored[:k]]


@mcp.tool()
def search_docs(query: str) -> str:
    """Answer a question about mcpvessel's commands, flags, security model, or
    usage by searching its documentation on GitHub. Use this first."""
    try:
        hits = _rank(_load_index(), query)
    except Exception as exc:
        return (
            f"Could not reach the mcpvessel docs on GitHub: {exc}. The cage must "
            "allow api.github.com and raw.githubusercontent.com (run or serve with "
            "--egress api.github.com,raw.githubusercontent.com)."
        )
    if not hits:
        return "No matching documentation. Try search_code for how it is implemented."
    return "\n\n---\n\n".join(f"## {c['crumb']}  ({c['source']})\n{c['text'][:1500]}" for c in hits)


@mcp.tool()
def search_code(query: str) -> str:
    """Search mcpvessel's source on GitHub for how something is implemented, when
    the docs do not answer. Needs the GITHUB_PERSONAL_ACCESS_TOKEN secret because
    GitHub code search requires authentication."""
    if not os.environ.get("GITHUB_PERSONAL_ACCESS_TOKEN", ""):
        return "Code search needs a GitHub token: pass --secret GITHUB_PERSONAL_ACCESS_TOKEN. Use search_docs for documented behavior."
    try:
        q = urllib.parse.quote(f"{query} repo:{REPO}")
        res = json.loads(_get(
            f"https://api.github.com/search/code?q={q}&per_page=5",
            accept="application/vnd.github.text-match+json",
        ))
    except Exception as exc:
        return f"Code search failed: {exc}"
    items = res.get("items", [])
    if not items:
        return "No matching source found."
    out = []
    for item in items[:5]:
        frags = [m.get("fragment", "") for m in item.get("text_matches", [])]
        out.append(f"### {item.get('path', '')}\n" + "\n...\n".join(frags[:2]))
    return "\n\n---\n\n".join(out)


def _frontmatter_value(text, key):
    m = re.search(rf'(?m)^\s*{key}:\s*"?([^"\n]+?)"?\s*$', text)
    return m.group(1) if m else ""


@mcp.tool()
def latest_skill(client_id: str = "claude-code") -> str:
    """Return the latest mcpvessel skill for an MCP client, fetched live from the
    repository, so an agent can keep its installed skill current without upgrading
    the mcpvessel binary. The skill (in this repo at
    internal/clientskill/skills/<client_id>/SKILL.md) evolves faster than the
    binary; this serves whatever is on the default branch right now.

    The returned text is the SKILL.md itself, including a metadata block with
    `version` and `requires` (the minimum mcpvessel binary it needs). Apply an
    update only when BOTH hold: this version is greater than your installed
    skill's metadata.version, and `mcpvessel --version` is at least `requires`.
    Then write it with `mcpvessel skill install --from -` and start a new session.
    If `requires` exceeds the installed binary, do not apply it; tell the user the
    newest skill needs a binary upgrade for its new commands."""
    path = f"internal/clientskill/skills/{client_id}/SKILL.md"
    try:
        content = _get(f"https://raw.githubusercontent.com/{REPO}/{BRANCH}/{path}")
    except Exception as exc:
        return (
            f"Could not fetch the latest skill for {client_id!r}: {exc}. Known "
            "client id: claude-code. The cage must allow raw.githubusercontent.com."
        )
    version = _frontmatter_value(content, "version")
    requires = _frontmatter_value(content, "requires")
    return (
        f"latest {client_id} skill: version {version or '?'}, "
        f"requires mcpvessel >= {requires or '?'}.\n"
        "Compare to your installed skill's metadata.version and your "
        "`mcpvessel --version`; apply with `mcpvessel skill install --from -` only "
        "if newer and compatible. The full SKILL.md follows.\n\n"
        + content
    )


if __name__ == "__main__":
    mcp.run()
