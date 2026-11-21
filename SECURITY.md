# Security

This server handles access to people's health data, so security reports are
very welcome.

**Please don't open a public issue for a vulnerability.** Report it privately
through GitHub: **Security → Report a vulnerability** on this repository.
Include what you found, how to reproduce it, and what an attacker could do.

You'll get a reply as soon as possible. This is a spare-time project, so
please allow a few days.

Things that are especially worth a look:
- the OAuth flow in `internal/remote` (client registration, the consent page
  and its browser binding, PKCE, token sealing)
- anything that could expose one user's data or Google token to another user
  or to a client they didn't approve
