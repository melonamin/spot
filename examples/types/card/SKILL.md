# Card

A card instance has one data file, `card.json`, and optional images under
`media/`. Write only those paths; never write `index.html` or `_type/`.

`card.json`:

```json
{
  "title": "Quarterly review",
  "subtitle": "October 2026",
  "body": "One short paragraph.",
  "points": ["First point", "Second point"],
  "image": "cover.png",
  "imageAlt": "Team photo"
}
```

- `title` is required; every other field is optional.
- `image` names a file in `media/` (letters, digits, `.`, `_`, `-`).
- Publish the data directory with `type=card@1.0.0`.
