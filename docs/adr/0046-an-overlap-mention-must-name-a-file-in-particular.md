# ADR-0046: An overlap mention must name a file in particular, and a held queue says so

- Status: Accepted
- Date: 2026-11-10
- Amends: ADR-0032

## Context

On 2026-11-09 the herd ticket loop logged `queue empty; re-polling` for hours
while about 20 ready tickets waited (#281). The overlap screen (ADR-0032) had
deferred every one of them, and with the default `OVERLAP_THRESHOLD=1` one false
match was enough. Three of ADR-0032's prediction rules matched far more than the
ticket meant:

- A dot-prefixed name has an empty stem. `` `*.figma.tsx` `` tokenises to
  `.figma.tsx`, whose stem `""` is the stem of every dotfile, so one claimed
  ticket "named" 103 files and held unrelated tickets behind it.
- A bare file name names every file with its stem, anywhere in the tree. A
  generic name (`index.ts`, `config`, `types`) overlaps almost any PR.
- A path names every file under it, so a ticket that names a folder as context
  waits behind any PR that touches one file there.

A file name quoted in a log excerpt or a stack trace deferred a ticket too,
though it is evidence of a failure, not a plan to edit the file.

Then the loop hid the cause: an empty selection read as "queue empty", and the
`deferred:` lines reached only the console.

## Decision

**A mention defers a ticket only when it names a file in particular, and a queue
the screen holds whole is narrated as held, with the deferrals in the stream.**

- **A directory names no file.** A path names the file it is, and any file it is
  the tail of. It no longer names every file under it.
- **A bare file name or backticked module name with an empty or generic stem
  names nothing.** Generic is a short stoplist (`overlap.genericStems`: `index`,
  `main`, `route`, `routes`, `config`, `types`, `env`, `utils`, `util`,
  `helpers`, `constants`, `page`, `layout`). Such a file is named by its path. A
  distinctive name still names every file with its stem, so `controls.tsx`
  still names `controls.test.tsx`.
- **A fenced code block names nothing.** Its files are evidence, not a plan.
- **A held queue says so.** When the screen defers every ready ticket,
  `pipeline --next` logs `no eligible ticket — queue held: N ticket(s) deferred
  by overlaps (<keys>)`, and the loop's idle record says `queue held: …` in place
  of `queue empty` (`loop.Holder`). Each deferral goes to `loop.jsonl` as an
  `item-skipped` record the first time it stands, and only to the console on
  later polls while its reason is unchanged.

## Alternatives considered

- **Count a directory mention only together with a file-level one.** When a
  ticket names a directory and a file in it, the file already defers the ticket,
  so this only adds files to the reason. It is not worth a second kind of
  mention.
- **Match a bare file name on its whole base name, not its stem.** That still
  matches every `index.ts` in a web app, and it drops the deliberate match of a
  file's tests and stories.
- **Also skip Markdown block quotes.** A quote is as often a spec being quoted
  as a log, so only fenced blocks are skipped.

## Consequences

- A ticket that names only a directory, a generic file name, or files inside a
  fenced block no longer waits for the work in flight. If it does collide, the
  cost is a conflict a human fixes, as before ADR-0032. Name the file by its path
  to make a ticket wait.
- A ticket that names a distinctive file, or a file by its path, still waits
  behind the PR that changes it.
- A loop idling over a full but held queue names the held tickets on every idle
  line, and the stream records why each one waits.
