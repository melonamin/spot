# Spot Show schema

Spot Show is the preferred format for agent-authored visual reports on Spot. A
show is a JSON document rendered into a static site by:

```sh
spot show deploy <site-name> show.json
```

Reuse the same site name while iterating. Open browser tabs refresh after each
redeploy through `/spot-live.js`. The generated `_spot.json` uses the show's
`title` and `description` for gallery metadata.

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
  "title": "Implementation plan",
  "summary": "Short card intro.",
  "compact": false,
  "blocks": []
}
```

Fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
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
| `kind` | string | yes | One of `markdown`, `mermaid`, `diff`, `terminal`, `code`, `json`, `image`, `html`. |
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

### `diff`

Use for code review or showing a patch.

```json
{
  "kind": "diff",
  "title": "Server changes",
  "body": "diff --git a/server.go b/server.go\n@@\n- old\n+ new"
}
```

Pass unified/git diff text in `body`.

### `terminal`

Use for commands, test output, logs, and deploy transcripts.

```json
{
  "kind": "terminal",
  "title": "Validation",
  "body": "$ go test ./...\nok  ./server"
}
```

### `code`

Use for focused snippets where the code itself is the point.

```json
{
  "kind": "code",
  "title": "renderer.js",
  "language": "js",
  "body": "export function render(show) { return show.cards; }"
}
```

Fields:

| field | type | required | meaning |
| --- | --- | --- | --- |
| `body` | string | yes | Source code. |
| `language` | string | no | Language hint such as `js`, `ts`, `go`, `python`. |

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
or use a URL reachable by the browser.

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

## Agent workflow

1. Fetch this schema before authoring a Spot Show:

   ```sh
   curl -fsSL "$SPOT_URL/spot-show-schema.md"
   ```

2. Write or update `show.json`. To create a starter file:

   ```sh
   spot show init show.json
   ```

3. Deploy the same site name:

   ```sh
   spot show deploy <site-name> show.json
   ```

4. Tell the user the URL once. On later updates, just redeploy and summarize what
   changed. For active local iteration, use:

   ```sh
   spot show watch <site-name> show.json
   ```

5. Prefer updating the existing show over creating a new site.

## Safety notes

- Do not include secrets, credentials, private tokens, or sensitive source in a
  show unless the site is appropriately restricted and downloads are acceptable.
- Use `html` only when the other block kinds cannot express the idea.
- Keep each card focused; split unrelated ideas into separate cards.
