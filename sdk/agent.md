# Spot Agent Setup

You are working with a Spot server. Install the Spot CLI, initialize the agent
skill, then consult the current Spot-specific instructions from this running
server.

Use the origin that served this document. For example, if this document URL is
`https://spot.corp.example.com/agent.md`, run:

```sh
curl -fsSL https://spot.corp.example.com/install.sh | sh -s -- https://spot.corp.example.com
```

Then, inside the project:

```sh
spot init --yes
```

Before building/deploying Spot sites or authoring Spot Show visual reports, fetch
the current agent how-to from the same Spot origin:

```sh
spot agent-howto
```

If the CLI is unavailable, fetch it directly. For example, if this document is at
`https://spot.corp.example.com/agent.md`:

```sh
curl -fsSL https://spot.corp.example.com/spot-agent-howto.md
```

For Spot Show card/block reports, also fetch the schema before writing
`show.json` or running `spot show init`:

```sh
spot show-schema
# or:
curl -fsSL https://spot.corp.example.com/spot-show-schema.md
```

The fetched how-to covers the intended workflow, when to use Spot Show versus a
custom site, supported block kinds, feedback iteration, and safety notes. The
schema document defines the required JSON shape for cards and blocks.
