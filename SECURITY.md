# Security Policy

We take the security of pulsys seriously. Thank you for helping keep it and
its users safe.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, or pull requests.**

Instead, use GitHub's private vulnerability reporting:

1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability** (this opens a private
   [GitHub Security Advisory](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)).

Please include, where possible:

- the affected version / commit and component (data plane, admin plane, chart, CDK);
- a description of the issue and its impact;
- steps to reproduce or a proof of concept;
- any suggested remediation.

## What to expect

- **Acknowledgement** within 3 business days.
- A **triage assessment** (severity + affected versions) within 7 business days.
- Coordinated disclosure: we will work with you on a fix and a disclosure
  timeline, and credit you in the advisory unless you prefer to remain anonymous.

## Supported versions

While the project is pre-1.0 (`v0.x`), security fixes land on `main` and in the
latest minor release. Pin to a released tag and watch releases to stay current.

## Scope and hardening

pulsys is built to sit **behind a hardened reverse proxy / load balancer**. The
full security engineering detail lives in [`docs/security.md`](docs/security.md):
the required production topology and the controls the proxy owns (slowloris/idle
timeouts, per-IP caps, body caps, parser hardening), the rationale for the
hand-rolled HTTP/1.1 server and how its parser is held no-looser than the Go
standard library, the CVE remediation table, the comparable-CVE threat model, the
provenance and licensing of all test material, and the supply-chain posture
(signing, SBOMs, scanning).

The HTTP/1.1 parser has a black-box regression suite for request smuggling,
header attacks, and resource-exhaustion in
[`internal/security/`](internal/security/). Reports that strengthen those
invariants are especially welcome.
