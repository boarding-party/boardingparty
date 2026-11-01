# Vendored JavaScript

Pinned ES module builds, served as-is from the embedded UI. No build step.
`index.html` maps the bare specifiers `preact`, `preact/hooks` and `htm` to
these files with an import map (`hooks.module.js` imports `"preact"`).

| File | Version | Source | SHA-256 |
| --- | --- | --- | --- |
| `vendor/preact.module.js` | preact 10.24.3 | https://unpkg.com/preact@10.24.3/dist/preact.module.js | `9ba8bb591b0c9a72132ff65a76e69c5a4e49cf71721b47b659f3243658bde05b` |
| `vendor/hooks.module.js` | preact 10.24.3 (`preact/hooks`) | https://unpkg.com/preact@10.24.3/hooks/dist/hooks.module.js | `896fc8e546b96c3fca29743b493293820ad4e76396fd36ff05f18a52eaf303e1` |
| `vendor/htm.module.js` | htm 3.1.1 | https://unpkg.com/htm@3.1.1/dist/htm.module.js | `ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4` |

To update: download the new versions from the same paths, replace the files,
and update this table (`shasum -a 256 vendor/*.js`).
