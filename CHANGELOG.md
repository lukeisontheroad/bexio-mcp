# Changelog

## [0.2.0](https://github.com/lukeisontheroad/bexio-mcp/compare/v0.1.0...v0.2.0) (2026-10-20)


### Features

* pin a connector to one bexio company by name ([#4](https://github.com/lukeisontheroad/bexio-mcp/issues/4)) ([d267792](https://github.com/lukeisontheroad/bexio-mcp/commit/d267792a9b990ab5f481f16c69f37843daf7c73e))
* set each area to off, read or read and write ([#5](https://github.com/lukeisontheroad/bexio-mcp/issues/5)) ([c3effdf](https://github.com/lukeisontheroad/bexio-mcp/commit/c3effdfc1bec76bf5cbab49993da3928286ccf74))

## 0.1.0 (2026-10-15)


### ⚠ BREAKING CHANGES

* needs BEXIO_CLIENT_ID and BEXIO_CLIENT_SECRET, the redirect URI /oauth/callback on the bexio app, and existing connectors must be re-added. The URL options (/readonly, module lists) are gone; the consent page replaces them.

### Features

* act as the clients' authorization server with a consent page ([770ee05](https://github.com/lukeisontheroad/bexio-mcp/commit/770ee05aa909a3841c2739e601f90ee30824a857))
* log requests and bexio statuses without tokens or bodies ([55c4055](https://github.com/lukeisontheroad/bexio-mcp/commit/55c4055a094e7a2e4489fd154b43d5f7d1d84ce6))
* make the connector label optional ([6908b6d](https://github.com/lukeisontheroad/bexio-mcp/commit/6908b6d9d3fb576dbcc2eca27fa88d0ba2b24aee))
* optional Sentry error reporting with scrubbed events ([4875313](https://github.com/lukeisontheroad/bexio-mcp/commit/4875313ae8c79a8c1d5e571e06ed3f2f2b50e064))
* pick modules and read-only per connector URL ([df5acc4](https://github.com/lukeisontheroad/bexio-mcp/commit/df5acc47e3fc612c4db1d99d5e214832bd194bfa))
* stateless remote MCP server for the bexio API ([820a96a](https://github.com/lukeisontheroad/bexio-mcp/commit/820a96a5077bb331e61e3d25371afd83c860a3d8))


### Bug Fixes

* **oauth:** bind sign-ins to the browser and close pre-consent redirects ([aa46de2](https://github.com/lukeisontheroad/bexio-mcp/commit/aa46de23dfe00a6b16bab7ade0b3e7716c18025a))
* **oauth:** no redirect on cancel, strict redirect schemes, __Host- flow cookie ([8ee1605](https://github.com/lukeisontheroad/bexio-mcp/commit/8ee160526c614edb624fbdb3e60bb1b4e0d48294))
* refuse connector options without a label ([d3a47dd](https://github.com/lukeisontheroad/bexio-mcp/commit/d3a47dd0e331da75f5a95f8ab355502a12a3d04f))
