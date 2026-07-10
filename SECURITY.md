# Security policy

agentcage runs agents in isolated containers behind policy gateways, with a
signing and trust model over the artifacts. A hole in the sandbox or the trust
model is the most serious kind of bug it can have. Reports are welcome and taken
seriously.

## Reporting a vulnerability

Please report privately, not in a public issue:

- Open a [GitHub private security advisory](https://github.com/okedeji/agentcage/security/advisories/new), or
- Email **tobiokedeji@gmail.com** with `agentcage security` in the subject.

Include what you need to reproduce it: the Agentfile or bundle, the commands,
and what you expected versus what happened. A proof of concept helps but is not
required.

This is a solo-maintained project, so please allow a few days for an initial
reply. Once a fix is out, credit is given in the release notes unless you would
rather stay anonymous.

## Scope

In scope, most valuable first:

- Sandbox escape: an agent reaching the host, another cage, or the network it
  was not granted.
- Gateway bypass: reaching a denied or banned tool, or a sub-agent a `BAN`
  should have blocked.
- LLM key or secret exposure to an agent, or spend that escapes the budget.
- Signature or trust bypass: a bundle verifying under the wrong key, or a
  key-mismatch that does not fail closed.
- Egress bypass: reaching a host outside an `EGRESS allow:` list, or SSRF
  through the egress proxy.

Out of scope:

- The known limits of signing: the first pull of a publisher trusts the key it
  sees (trust on first use), and signing proves origin, not intent. The sandbox,
  not the signature, is what contains a malicious agent.
- Denial of service from an agent you deliberately ran with generous caps.
- Anything requiring you to already hold the host user's credentials or root.

## Supported versions

Pre-1.0, only the latest tagged release gets fixes. Pin a digest if you need a
stable artifact.
