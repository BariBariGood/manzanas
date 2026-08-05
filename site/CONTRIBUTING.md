# Contributing to the site

## Turning findings into "Field notes" posts

The blog's Field notes series turns real, measured engineering findings into
short posts. If you've discovered something genuinely surprising while working
on the fleet or the daemon, it probably belongs here.

### What makes a post

- **A real measurement.** Every post is anchored on numbers we actually
  measured: before/after CPU, latencies, byte counts, build times. No
  estimates, no vibes.
- **A surprise.** The best posts have an "aha" — an assumption that turned out
  wrong, a fix that lived somewhere unexpected, an old tool solving a new
  problem. If the finding is routine, it belongs in `docs/`, not the blog.
- **Short and punchy.** No fluff — straight to the point, simply summarized.
  A terse list style works best: one short section (2–4 sentences) per
  finding, each anchored on its key measured numbers.
- **Max one post per day.** If several findings land on the same day,
  consolidate them into a single post with one short section per finding.

### Mechanics

1. Create `app/blog/<slug>/page.tsx`. Copy the structure of an existing Field
   notes post (e.g. `app/blog/field-notes-aug-2/page.tsx`): `metadata` export,
   then `FieldNote` (date), `h1`, and `H2`/`P` sections.
2. Use the shared components from `components/BlogKit.tsx` — `H2`, `P`, `Bar`,
   `StatCard`, `FieldNote` — and `components/CopyBlock.tsx` for terminal
   snippets. Prefer a `StatCard` with `Bar`s for any before/after comparison;
   the numbers are the post.
3. Add the post to the `posts` array in `app/blog/page.tsx` (newest first)
   with `fieldNote: true`, a date, and a one-or-two-sentence teaser that leads
   with the most surprising number.
4. Run `npm run build` in `site/` and check the post renders.

### Content rules

- Anonymize hardware: "an M3 Pro", "a 2013 Mac Pro", "two 2017 MBPs" — never
  hostnames, IPs, usernames, or credentials.
- Real numbers only, quoted from measurements recorded in the repo's docs or
  PR descriptions.
- Match the site's voice: confident, concrete, a little wry. Look at the
  existing posts before writing.
