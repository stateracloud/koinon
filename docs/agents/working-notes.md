# Working notes

Maintainers' working notes (an agent's memory directory, pending and lane logs) grow without bound
unless something retires them. This is the convention (#527); `hack/notes-expiry.sh` enforces it.

## Memory entries

Every memory file is Markdown with YAML frontmatter. Besides `name`, `description` and the type, it
carries exactly one of:

- `expires: YYYY-MM-DD`, by default 30 days after the note was written or last confirmed. Dated
  decisions, handoffs, "resume" notes and anything that describes a state of the world expire.
- `durable: true`, for standing rules and preferences that stay true until someone changes them
  (feedback, how a person wants to work, facts about a tool that do not rot).

`review_by: YYYY-MM-DD` is accepted as a synonym of `expires:`. A file with none of the three counts
as expired.

## Pending and lane logs

Append-only bullet logs (`- YYYY-MM-DD ~HH:MM ...`). A bullet older than 14 days is moved out
(into a memory file, an ADR, a doc or an issue) or deleted. Nothing is kept in a log for later.

## The nightly retro

The retro runs

```sh
hack/notes-expiry.sh <memory dir> --exclude MEMORY.md
hack/notes-expiry.sh --log <log file> --days 14
```

and lists what they print for the human. Each item gets one of three outcomes:

- keep: set a new `expires:` date, or `durable: true`;
- promote: move the content into an ADR, a doc or an issue, then delete the note;
- delete.

The script only lists. Exit 0 means nothing is due, 1 means something is listed, 2 means a usage
error. Output is one tab-separated line per item: path (or line number), date (`no-expiry` when the
file has no field) and the first 80 characters.
