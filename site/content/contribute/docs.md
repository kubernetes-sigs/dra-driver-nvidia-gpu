---
title: Contribute to Docs
linkTitle: Docs
weight: 15
description: Build and preview this site locally.
---

The site lives in
[`site/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/main/site)
and is built with [Hugo](https://gohugo.io/) (the extended build) and the
[Docsy](https://www.docsy.dev/) theme.

## Quick start

You need [Hugo (extended)](https://gohugo.io/installation/),
[Go](https://go.dev/doc/install), [Node.js](https://nodejs.org/) (which
provides `npm`), and [Git](https://git-scm.com/downloads) installed. The versions on your machine should meed the minium required versions listed in
[`netlify.toml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/netlify.toml).

```bash
git clone https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu.git
cd dra-driver-nvidia-gpu/site
npm ci          # one-time; installs the locked site build dependencies
npm run serve   # http://localhost:1313, live-reload, drafts visible
```

Pagefind creates its search index after Hugo finishes, so search is not
available from the live-reload server. To build the site with drafts and future
content, create the Pagefind index, and start a local server for testing search,
run:

```bash
npm run preview-search
```

Open the URL printed by Pagefind. Stop the server with `Ctrl+C`. Re-run the
command after changing content that you want to test in search.

To produce the same output Netlify builds for production, from `site/`:

```bash
npm run build   # Hugo output and Pagefind index written to ./public
```

## Updating docs for a release

When a new release is cut, the docs changes are:

1. **Update the version params in [`site/hugo.toml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/hugo.toml)**. Both values must be updated together:

   ```toml
   driver_version = "0.4.1"       # Helm chart version (no "v" prefix)
   driver_release_tag = "v0.4.1"  # GitHub release tag
   ```

   These params are embedded in install command examples across the docs via `{{</* param "driver_version" */>}}`. Updating them here updates every code sample automatically.

2. **Update [`site/content/docs/install.md`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/content/docs/install.md)** if the install procedure changed (new flags, new prerequisites, updated verification steps).

3. **Update [`site/content/docs/upgrade.md`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/content/docs/upgrade.md)**. For a patch release (x.y.Z), add a short new section describing the upgrade from the previous version. For a minor or major release (x.Y.0), the existing upgrade content can be revised in place.

4. **Update any concept or guide pages** that describe behavior that changed in the release.

For reference, the release notes live in [`release-notes/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/main/release-notes) at the repo root.

## Using version site variables in docs

Two version params are defined under `[params]` in
[`site/hugo.toml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/hugo.toml):

| Param | Example value | Use for |
| --- | --- | --- |
| `driver_version` | `0.4.1` (no `v` prefix) | Helm `--version` flags and any bare chart version |
| `driver_release_tag` | `v0.4.1` (with `v` prefix) | GitHub source links to a tagged path |

Use these params anywhere the docs refer to the current release:

- **`driver_version`** — any command that takes `--version`, or prose that names the chart version.
- **`driver_release_tag`** — any link to files or directories at a tagged release (for example `tree/{{< param driver_release_tag >}}/demo`), or prose that names the GitHub release tag.

Do not hardcode version strings in docs content unless you are calling out behavior that is specific to a particular driver version (for example an upgrade note that applies only when moving from `0.3.x` to `0.4.0`).

Reference the param with the Docsy `param` shortcode.

In prose:

```text
Install driver release {{</* param driver_release_tag */>}} with chart version {{</* param "driver_version" */>}}.
```

In a fenced code block:

```text
helm install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{</* param "driver_version" */>}} \
    --create-namespace \
    --namespace dra-driver-nvidia-gpu \
    --set gpuResourcesEnabledOverride=true
```

For a link to a tagged source file, place `{{</* param driver_release_tag */>}}` in the GitHub URL after `/blob/`:

[`values.yaml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/{{< param driver_release_tag >}}/deployments/helm/dra-driver-nvidia-gpu/values.yaml)

When adding syntax examples to this contribute page, escape shortcode delimiters in `text` code blocks (`{{</* ... */>}}`) so Hugo shows the source literally instead of substituting the current version.

## Where to look for more details

| If you need to know… | Look at |
| --- | --- |
| Exact Hugo / Go / Node versions used for production builds | [`netlify.toml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/netlify.toml) |
| Minimum Hugo version enforced locally, plus all site configuration | [`site/hugo.toml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/hugo.toml) |
| Docsy theme version this site pins | [`site/go.mod`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/go.mod) |
| npm scripts and PostCSS dependencies | [`site/package.json`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/site/package.json) |
| How to install Hugo on any OS (use the extended build) | [Hugo installation docs](https://gohugo.io/installation/) |
| Docsy theme prerequisites, configuration, and authoring guide | [Docsy documentation](https://www.docsy.dev/docs/) |
