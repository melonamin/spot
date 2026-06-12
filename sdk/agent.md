# Spot Agent Setup

You are working with a Spot server. Install the Spot CLI, initialize the
project skill, then deploy static sites through this server.

Use the origin that served this document. For example, if this document
URL is `https://spot.corp.example.com/agent.md`, run:

```sh
curl -fsSL https://spot.corp.example.com/install.sh | sh -s -- https://spot.corp.example.com
```

Then, inside the project:

```sh
spot init
spot deploy <site-name>
```

Spot sites are folders with an `index.html`. Plain HTML, CSS, and JS work
without a build step. Load the browser SDK with:

```html
<script src="/spot.js"></script>
```

After `spot init`, read `.claude/skills/spot/SKILL.md` before building or
deploying Spot sites. The skill documents identity, database, realtime,
AI, file uploads, access control, and deployment conventions.
