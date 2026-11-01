# Vendored JavaScript

Pinned ES module builds in `ui/vendor/`, served as-is from the embedded UI. No
build step and no import map: the board's CSP forbids inline scripts, so
`app.js` imports the files by relative path.

| File | Version | Source | SHA-256 (upstream) | SHA-256 (as vendored) |
| --- | --- | --- | --- | --- |
| `ui/vendor/preact.module.js` | preact 10.24.3 | https://unpkg.com/preact@10.24.3/dist/preact.module.js | `9ba8bb591b0c9a72132ff65a76e69c5a4e49cf71721b47b659f3243658bde05b` | same |
| `ui/vendor/hooks.module.js` | preact 10.24.3 (`preact/hooks`) | https://unpkg.com/preact@10.24.3/hooks/dist/hooks.module.js | `896fc8e546b96c3fca29743b493293820ad4e76396fd36ff05f18a52eaf303e1` | `66d649cc2f76c6f038737b495dd2102f7f3c4a9aee1b8b01eb1ee87e293e13b4` |
| `ui/vendor/htm.module.js` | htm 3.1.1 | https://unpkg.com/htm@3.1.1/dist/htm.module.js | `ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4` | same |

## Local patch

`hooks.module.js` imports the bare specifier `"preact"`. Without an import map
the browser can't resolve it, so the one import is rewritten:

```
sed -i '' 's/from"preact"/from".\/preact.module.js"/' ui/vendor/hooks.module.js
```

No other changes.

## Updating

Download the new versions from the same paths, re-apply the patch above, check
there are no other bare imports (`grep -o 'from"[^"]*"' ui/vendor/*.js`), and
update both hash columns (`shasum -a 256 ui/vendor/*.js`).
