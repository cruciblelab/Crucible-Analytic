# Vendored third-party assets

Everything the panel serves to a browser is in this directory and is
compiled into the binary. There is no CDN, no npm, and no build step:
deploying the panel is copying one file.

## htmx.min.js

- Upstream: <https://github.com/bigskysoftware/htmx>
- Version: **2.0.10**
- Fetched from: `https://unpkg.com/htmx.org@2.0.10/dist/htmx.min.js`
- SHA-256: `71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de`
- Licence: BSD Zero Clause (0BSD)

The hash is asserted by `TestVendoredHTMXMatchesItsRecordedHash` in
`assets_test.go`. That test is not ceremony: this file is the one thing
in the repository nobody reads during review, so an edit to it — an
accident, a bad merge, or somebody pasting a "patched" build — would
otherwise land unnoticed. Changing the version means changing the hash
in the test *and* the version here, deliberately, in the same commit.

To update:

```sh
curl -fsSL -o internal/panel/ui/static/htmx.min.js \
  https://unpkg.com/htmx.org@<version>/dist/htmx.min.js
openssl dgst -sha256 -hex internal/panel/ui/static/htmx.min.js
```

Then update the version and hash above and in `assets_test.go`, and read
htmx's release notes — the CSP the panel sends (`ui/headers.go`) allows
neither `unsafe-inline` nor `unsafe-eval`, so a release that starts
requiring either would break the panel rather than silently loosen it.
