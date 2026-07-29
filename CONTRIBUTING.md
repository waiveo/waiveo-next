# Contributing

Thanks for your interest. Right now:

- **Issues / bug reports / feature requests:** open — please use the templates.
- **Pull requests:** not yet accepted from external contributors. A
  Contributor License Agreement (CLA) with a signing check lands here before
  the first external PR is merged; until then, external PRs will be closed
  with a pointer to this file.

Local toolchain: Node version per `.node-version`, latest stable Go.
Run `make dev` for the development stack and `node scripts/validate-contracts.mjs`
before pushing contract changes.

The dev stack's API is authenticated, so the HTTP probes need a local API key.
`make dev` provisions one for you; run `make dev-key` by hand if you drive the
probes outside make. The key is git-ignored, never printed, and what it is
authorized to do — and why — is documented in `scripts/devcred`.
