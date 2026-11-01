# Security Policy

## Supported Versions

Only the latest release receives security fixes:

| Version | Supported |
|---|---|
| latest tagged release | yes |
| older releases | no |
| `main` | yes (best effort) |

## Reporting a Vulnerability

Please report vulnerabilities privately using
[GitHub private vulnerability reporting](https://github.com/lucaswilliameufrasio/multi-source-repo-sync/security/advisories/new).
Do not open a public issue for anything you believe is exploitable.

Include, when possible:

- affected version or commit;
- how you run it (command line, `serve`, `resolve`);
- impact and a minimal reproduction;
- any relevant output (redact tokens and private repository names).

You can expect:

- an initial response within 72 hours;
- a fix or a mitigation plan, with credit if you want it;
- coordinated disclosure after a patched release.

## Scope Notes

- **What it can write.** It writes only through your own `gh` and Git credentials, and only a new branch and a pull request, after an exact typed confirmation. A way to make it merge, force-push, delete, overwrite an existing branch, or write without that confirmation is a vulnerability.
- **`serve`** listens only on a loopback address and asks for a random token. A way to reach it from another machine or another web page (DNS rebinding, CSRF, a missing check of `Host` or `Origin`), to read the token, or to apply a sync without the confirmation is a vulnerability.
- **`resolve check`** runs a command you approve, on your machine, with your permissions and no sandbox. That is documented and out of scope; running it on code you did not review is the user's choice.
- It never reads or stores a token. If you find a secret in the repository or in a release artifact, treat it as a vulnerability and report it.
