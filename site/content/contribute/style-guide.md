---
title: Documentation style guide
linkTitle: Writing style
weight: 16
description: Write clear, accurate procedures and examples for the DRA driver.
---

Use this guide when writing or reviewing human-facing documentation in this repository, including the documentation site, README files, and demo instructions.
It applies to changed prose, examples, and page structure; preserve literal API names, commands, output, and generated content.
Keep edits focused on the task at hand.

## Write for the reader

- State what the reader can do or learn at the start of a page or section.
  Put prerequisites before the action that needs them.
- Prefer active voice and name the actor.
  Write "Kubernetes creates a `ResourceClaim`" rather than "A `ResourceClaim` is created."
- Use present tense for behavior: "The command lists the devices" rather than "The command will list the devices."
  Use the imperative for steps: "Apply the manifest."
- Address the reader as "you" when it helps.
  Give one action per step and use the same term for the same concept throughout a page.
- Define an uncommon abbreviation on first use.
  Keep API kinds, field names, feature gates, flags, and command names exactly as implemented.
- Choose concrete words over vague claims such as "easy," "robust," or "works."
  State the condition or observable result instead.

## Check technical claims

- Check commands, defaults, supported behavior, and configuration fields against the code, Helm chart, tests, or runnable examples.
  Identify required hardware, software, permissions, and feature gates.
- Distinguish supported behavior from experimental or feature-gated behavior.
  State meaningful limits and failure conditions where a reader needs them to complete the task.
- Keep one source for a long runnable manifest, preferably under `demo/`, and link to it from the guide.
  Label partial YAML as an excerpt so readers do not mistake it for a complete manifest.
- Use the [release version parameters](/contribute/docs/#using-version-site-variables-in-docs) instead of repeating the current driver version in commands and source links.

## Organize a page

- Give site pages a useful front matter `title` and `description`.
  Do not repeat the title as a body H1.
- Start with a short description of the task or concept.
  Use sentence-case headings that describe the reader's task, with H2 sections before H3 subsections.
- Keep one procedure in one place.
  Link to that procedure from related pages rather than copying steps that can drift apart.
- Introduce lists, code blocks, and tables with enough context to explain what the reader should do with them.
- Prefer one sentence per source line in new prose, including list items, so human maintainers can review sentence-level changes in diffs.
  Keep front matter, code blocks, and literal output in the format their syntax requires.

## Write procedures

Before the first step, state the prerequisites and any effect the reader needs to consider, such as creating cluster resources or changing GPU configuration.
Then give numbered actions in the order they must run.
Include a command or observation that verifies success and its expected result.
Include cleanup or recovery when the procedure leaves resources behind or can fail partway through.

Begin each step with an action verb.
Keep the command, expected result, and any explanation inside the same numbered item.
In Markdown, indent a continuation by the width of its list marker: three spaces after `1. ` through `9. `, and four spaces after `10. `.
Leave a blank line before and after an indented code block.
For example:

````markdown
1. Apply the manifest:

   ```bash
   kubectl apply -f example.yaml
   ```

   Kubernetes creates the resources in the manifest.

2. Verify that the pod is ready:

   ```bash
   kubectl wait --for=condition=Ready pod/example --timeout=5m
   ```

   The command exits successfully when the pod is ready.
````

Preview a changed procedure to confirm that its commands and explanations render inside the intended step.
A fence at the left margin after a numbered item ends the list in many Markdown renderers.

## Format examples and links

- Use language tags such as `bash`, `yaml`, and `json` on code fences.
  Put only runnable commands in a `bash` block: omit shell prompts and put example output in a separate `text` block.
- Tell the reader where to run a command and where a referenced local file comes from.
  Keep shell commands out of `yaml` blocks.
- Use backticks for filenames, paths, flags, environment variables, API identifiers, and literal values.
  Use descriptive link text instead of "click here" or a bare URL in prose.
- Link to downloadable YAML or a raw file URL for `kubectl apply -f`; a GitHub `/blob/` URL serves a web page.
  If an example requires edits before it runs, state exactly which values to replace.

Prefer one command per code block.
Group a short sequence when readers should copy and run it as one task, the commands share shell state or a direct dependency, and no intermediate result requires inspection.
Ensure a failed command cannot cause later commands to act on the wrong state.
For example, an environment variable assignment can stay with the command that uses it; show an `ls` command used for verification and its expected output separately.
The absence of output alone is not a reason to group commands.

## Include a local YAML file

Embed a complete YAML file when readers benefit from seeing the whole runnable example on the page.
Keep the file in this repository and use it as the single source for both the example and the procedure.
If the file lives outside `site/assets/`, mount it into Hugo's `assets` component in `site/hugo.toml`.
When adding a module mount, explicitly preserve the default mounts for content, layouts, assets, static files, and other site components.

Use the `include-yaml` shortcode with the path under `assets`.
The following source embeds that complete file with YAML syntax highlighting:

```text
{{</* include-yaml src="examples/my-workload.yaml" */>}}
```

Keep the YAML valid and put explanations outside the file.
Do not wrap in a \`\`\`yaml code fence.
Do not embed selected line ranges from the source: a later edit can make the excerpt incomplete or misleading.
If you show a small, hand-written excerpt, state that it is partial.
For a long manifest, link to the downloadable file instead of filling the page with it.
If the source lives in another repository, link to a pinned version instead of fetching it during the site build.

Check the file with a YAML parser.
When the required cluster APIs are available, also validate it with `kubectl apply --dry-run=client -f <file>`.
Check that the file matches the driver version and feature gates described on the page; embedding a file means its later changes also change the rendered documentation.

Before requesting review, read the changed page as a reader would: follow the steps in order, check that every command has the needed context, and confirm that the stated result lets the reader tell whether it succeeded.
