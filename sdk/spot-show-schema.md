# Spot Show schema

Spot Show is the preferred format for agent-authored visual reports on Spot. A
show is a JSON document rendered into a static site by:

```sh
spot show deploy <site-name> show.json
```

Validate structure and local image references before building or deploying:

```sh
spot show validate show.json
```

Reuse the same site name while iterating. Open browser tabs refresh after each
redeploy through `/spot-live.js`. The generated `_spot.json` uses the show's
`title` and `description` for gallery metadata. `spot show deploy` captures and
uploads a root `_screenshot.png` by default so the public gallery shows a real
preview; use `--no-screenshot` only when Chromium is unavailable or the user
explicitly wants to skip gallery previews.

## Top-level shape

```json
{
  "title": "Auth refactor review",
  "description": "What changed and why.",
  "updated_at": "2026-06-30 21:15:00",
  "cards": []
}
```

Fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
| `title` | string | yes | Board title shown in the page header. |
| `description` | string | no | One-line context under the title. |
| `updated_at` | string | no | Display-only timestamp/status text in the header. |
| `cards` | array | yes | Ordered cards. Keep each card focused on one idea. |

## Card shape

```json
{
  "id": "implementation-plan",
  "title": "Implementation plan",
  "summary": "Short card intro.",
  "compact": false,
  "blocks": []
}
```

Fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
| `id` | string | no | Stable card ID used by copyable `#card-...` links. Must match `[A-Za-z0-9][A-Za-z0-9_-]{0,63}`. |
| `title` | string | no | Card heading. Strongly recommended. |
| `summary` | string | no | Short card intro. |
| `description` | string | no | Alias/fallback for `summary`. |
| `compact` | boolean | no | Half-width card on wide screens; full-width on mobile. |
| `blocks` | array | yes | Ordered visual/content blocks. |

## Common block fields

Every block has a `kind` and may have a `title`:

```json
{ "kind": "markdown", "title": "Rationale", "body": "..." }
```

Common fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
| `kind` | string | yes | One of `markdown`, `mermaid`, `diff`, `terminal`, `code`, `json`, `image`, `html`, `trace`. |
| `title` | string | no | Label shown in the block header. |
| `body` | string | kind-dependent | Preferred text/content field. |
| `text` | string | no | Alias for `body` for text blocks. |
| `content` | string | no | Alias for `body` for text blocks. |

Prefer `body` unless you are adapting existing data.

## Block kinds

### `markdown`

Use for explanations, plans, tradeoffs, bullets, and prose.

```json
{
  "kind": "markdown",
  "title": "Summary",
  "body": "## Plan\n- Move token exchange server-side\n- Add tests"
}
```

Supported markdown is intentionally simple: headings, paragraphs, lists,
blockquotes, tables, links, bold text, inline code, and fenced code render well.
Raw HTML should use an `html` block instead.

### `mermaid`

Use for flows, architecture diagrams, sequence diagrams, and state diagrams.

```json
{
  "kind": "mermaid",
  "title": "Request flow",
  "body": "flowchart TD\n  A[Browser] --> B[Spot]\n  B --> C[SQLite]"
}
```

Diagrams follow the report's light/dark appearance and can be expanded into an
accessible fullscreen view. Prefer vertical `flowchart TD`/`TB` diagrams for
card readability. Mermaid runs in strict security mode.

### `diff`

Use for code review or showing a patch.

```json
{
  "kind": "diff",
  "title": "Server changes",
  "layout": "split",
  "body": "diff --git a/server.go b/server.go\n@@\n- old\n+ new"
}
```

Pass unified/git diff text in `body`. `layout` may be `unified` (the default) or
`split`. An unparseable split diff remains available as readable source rather
than dropping content.

### `terminal`

Use for commands, test output, logs, and deploy transcripts.

```json
{
  "kind": "terminal",
  "title": "Validation",
  "cols": 100,
  "body": "$ go test ./...\n\u001b[32;1mok\u001b[0m  ./server"
}
```

ANSI SGR colors and emphasis are supported. `cols` is optional and must be an
integer from 20 through 240; it expresses the intended terminal width without
forcing the card wider than the viewport. Cursor-addressing terminal controls
are intentionally ignored.

### `code`

Use for focused snippets where the code itself is the point.

```json
{
  "kind": "code",
  "title": "renderer.js",
  "language": "js",
  "line_start": 80,
  "body": "export function render(show) { return show.cards; }"
}
```

Fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
| `body` | string | yes | Source code. |
| `language` | string | no | Language hint such as `js`, `ts`, `go`, `python`. |
| `line_start` | positive integer | no | First displayed line number; defaults to `1`. |

Code displays copy-safe line numbers. Known languages are highlighted through a
pinned progressive browser dependency; plain escaped text remains readable if
the dependency is unavailable.

### `json`

Use when structured data matters more than prose.

```json
{
  "kind": "json",
  "title": "API response",
  "value": {
    "ok": true,
    "items": [1, 2, 3]
  }
}
```

Prefer parsed JSON in `value`. `body`/`data` are accepted fallbacks, but `value`
is clearest for agents.

### `image`

Use for screenshots or generated images.

```json
{
  "kind": "image",
  "title": "Preview",
  "src": "./screenshot.png",
  "alt": "Screenshot of the dashboard",
  "caption": "Dashboard after the layout pass."
}
```

Fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
| `src` | string | yes | Image URL/path. `url` is accepted as an alias. |
| `alt` | string | no | Accessible alt text. |
| `caption` | string | no | Caption under the image. |

If you reference a local image file, include it in the generated/deployed folder
or use a URL reachable by the browser. `spot show build` and `spot show deploy`
automatically copy local `image.src` files that stay within the directory
containing `show.json`, preserving their relative paths. Missing files, path
traversal, and symlink escapes fail validation. Images can be expanded
fullscreen in the generated report.

### `html`

Use sparingly for small custom or interactive demos.

```json
{
  "kind": "html",
  "title": "Mini demo",
  "body": "<style>body{font-family:system-ui}</style><button>Click me</button>"
}
```

HTML blocks render inside a sandboxed iframe. Do not put secrets in HTML; Spot
source may be downloadable by authorized viewers.

The iframe remains opaque-origin and resizes through a guarded message bridge.
Use these theme variables so a fragment follows the report without hard-coded
light/dark colors:

```css
--spot-bg
--spot-panel
--spot-panel-muted
--spot-line
--spot-text
--spot-muted
--spot-accent
--spot-accent-secondary
--spot-good
--spot-warn
--spot-bad
--spot-font-sans
--spot-font-mono
```

### `trace`

Use for a compact, expandable timeline of agent, tool, build, test, or deploy
steps.

```json
{
  "kind": "trace",
  "title": "Agent run",
  "steps": [
    {
      "label": "Run the test suite",
      "kind": "test",
      "status": "done",
      "detail": "just test passed",
      "ts": "10:42"
    }
  ]
}
```

`steps` must be a non-empty array. Every step requires a non-empty `label`.
`kind`, `detail`, and `ts` are optional strings. `status` is optional and must be
one of `pending`, `running`, `done`, or `error`. Detail starts collapsed.

## Viewer controls

- Appearance offers system, light, and dark modes and remembers the choice.
- Every card receives a stable fragment link; set `card.id` when links must stay
  stable across title or ordering changes.
- Mermaid and image blocks include accessible fullscreen controls.
- Code and diff blocks include copy controls that copy source rather than line
  numbers or rendered decoration.

## Recommended card patterns

### Design/architecture review

```json
{
  "title": "Auth design",
  "description": "Current and proposed login flow.",
  "cards": [{
    "title": "Proposed flow",
    "blocks": [
      { "kind": "markdown", "body": "## Summary\nToken exchange moves server-side." },
      { "kind": "mermaid", "body": "flowchart LR\nBrowser --> Spot --> Provider" }
    ]
  }]
}
```

### Implementation checkpoint

```json
{
  "title": "Deploy event implementation",
  "cards": [{
    "title": "Changes and evidence",
    "blocks": [
      { "kind": "diff", "body": "diff --git ..." },
      { "kind": "terminal", "body": "$ just test\nok" }
    ]
  }]
}
```

### Status board

```json
{
  "title": "Migration status",
  "cards": [
    { "title": "Done", "compact": true, "blocks": [{ "kind": "markdown", "body": "- Schema added\n- Tests pass" }] },
    { "title": "Remaining", "compact": true, "blocks": [{ "kind": "json", "value": { "tasks": ["docs", "release"] } }] }
  ]
}
```

### As Was / To Be comparison

Any comparison of a current and a proposed state, such as as was or as is versus
to be, or before versus after, must separate the two sides by background color.
Tinted text or box borders alone are not enough. Label each side too, and keep
each side's color on every table, diagram and panel of the report.

Use amber for the current state and green for the proposed state. Amber does not
read as an error, as red would.

| Side | HTML blocks | Mermaid fill | Mermaid stroke |
| --- | --- | --- | --- |
| As Was | `color-mix(in srgb,var(--spot-warn) 12%,transparent)` | `#d9770626` | `#b45309` |
| To Be | `color-mix(in srgb,var(--spot-good) 14%,transparent)` | `#05966926` | `#047857` |

These values follow the report's light and dark appearance.

Markdown tables cannot set a cell background, so write a comparison table as an
`html` block, with the color on the whole column or row:

```json
{
  "kind": "html",
  "title": "Comparison",
  "body": "<style>table{width:100%;border-collapse:collapse;font:14px var(--spot-font-sans);color:var(--spot-text)}th,td{border:1px solid var(--spot-line);padding:7px 10px;text-align:left}.was{background:color-mix(in srgb,var(--spot-warn) 12%,transparent)}.tobe{background:color-mix(in srgb,var(--spot-good) 14%,transparent)}</style><table><tr><th></th><th class='was'>As Was</th><th class='tobe'>To Be</th></tr><tr><td>Login</td><td class='was'>Browser token</td><td class='tobe'>Server exchange</td></tr></table>"
}
```

In a diagram, put each side in its own `subgraph` and style the subgraph, so that
the background covers the side and not only its nodes. The `~~~` link keeps As Was
first, because Mermaid may otherwise swap the subgraphs:

```json
{
  "kind": "mermaid",
  "title": "Flow",
  "body": "flowchart TB\n  subgraph WAS[As Was]\n    A[Browser] --> B[Provider]\n  end\n  subgraph TOBE[To Be]\n    C[Browser] --> D[Spot] --> E[Provider]\n  end\n  WAS ~~~ TOBE\n  style WAS fill:#d9770626,stroke:#b45309\n  style TOBE fill:#05966926,stroke:#047857"
}
```

## Agent workflow

1. Fetch this schema before authoring a Spot Show:

   ```sh
   curl -fsSL "$SPOT_URL/spot-show-schema.md"
   ```

2. Write or update `show.json`. To create a starter file:

   ```sh
   spot show init show.json
   ```

3. Validate the document and referenced local images:

   ```sh
   spot show validate show.json
   ```

4. Deploy the same site name:

   ```sh
   spot show deploy <site-name> show.json
   ```

   The deploy should include `_screenshot.png` for the gallery thumbnail.

5. Tell the user the URL once. On later updates, just redeploy and summarize what
   changed. For active local iteration, use:

   ```sh
   spot show watch <site-name> show.json
   ```

6. Prefer updating the existing show over creating a new site.

## Safety notes

- Do not include secrets, credentials, private tokens, or sensitive source in a
  show unless the site is appropriately restricted and downloads are acceptable.
- Use `html` only when the other block kinds cannot express the idea.
- Keep each card focused; split unrelated ideas into separate cards.
