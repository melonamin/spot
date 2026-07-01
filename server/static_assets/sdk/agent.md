# Spot Agent Setup

You are working with a Spot server. Install the Spot CLI, initialize the
agent skill, then deploy static sites through this server.

Use the origin that served this document. For example, if this document
URL is `https://spot.corp.example.com/agent.md`, run:

```sh
curl -fsSL https://spot.corp.example.com/install.sh | sh -s -- https://spot.corp.example.com
```

Then, inside the project:

```sh
spot init --yes
spot deploy <site-name>
```

For agent visual reports, use Spot Show cards instead of hand-authoring a
whole page. Before authoring a Spot Show, fetch the current schema from this
Spot server:

```sh
curl -fsSL https://spot.corp.example.com/spot-show-schema.md
```

Use the same origin that served this document; for example, if this file is at
`http://spot.localhost:8080/agent.md`, fetch
`http://spot.localhost:8080/spot-show-schema.md`. Then write `show.json` and
deploy it:

```sh
spot show deploy <site-name> show.json
```

The schema document defines the required JSON shape, card fields, and supported
blocks: `markdown`, `mermaid`, `diff`, `terminal`, `code`, `json`, `image`, and
sandboxed `html`. Redeploy the same site name while iterating; open browser tabs
refresh via `/spot-live.js`.

Spot sites are folders with an `index.html`, or a single `index.html`
file. Plain HTML, CSS, and JS work without a build step. Load the
browser SDK with:

```html
<script src="/spot.js"></script>
```

After `spot init`, read the generated skill for the selected agent before
building or deploying Spot sites. It is written to
`<agent>/skills/spot/SKILL.md`, for example `.claude/skills/spot/SKILL.md`
or `.codex/skills/spot/SKILL.md`; `spot init` prints the exact path(s) it
wrote. The skill documents identity, database, realtime, text AI, AI image
generation, Slack notifications, file uploads, access control, and deployment
conventions.

Use `spot.slack.send({ channel, text, blocks, mrkdwn })` to post to Slack
through Spot's server-side bot token. `channel` is passed through as a Slack
channel name (`#signups`), channel ID (`C0123`), or user ID (`U0123`) for a DM.
`text` uses Slack mrkdwn, not CommonMark; pass `mrkdwn:false` for literal text.
`blocks` are forwarded unchanged, including image blocks that reference public
external image URLs. Spot file URLs are private to the mesh and Slack cannot
fetch them.

Slack sends are gated like AI. Set `SPOT_SLACK_ACCESS=visitors` globally, or
opt a restricted site in with `_access.json`:

```json
{ "allow": ["team-payments"], "slack": "visitors" }
```
