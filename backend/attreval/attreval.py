#!/usr/bin/env python3
"""Attribution eval: runs the backend's attribution preview (POST
.../attribute-speakers/preview - nothing stored) over a fixed chapter set
and scores the proposals against the stored speakers.

Stored speakers are only an approximate gold: Arise: Alpha's chapters were
listened through and corrected, Spire's Spite ch. 40-58 were hand-corrected,
but both still hold some wrong lines (spot-checked). Jake's Magical Market 3
is a drift check only (never corrected, some quotes still Narrator). The
reliable number is HAND-CORRECTED LINES: lines the reader fixed by hand
(arise_edited.txt, from the server log before paragraph_edits existed, plus
GET .../paragraph-edits). An identical re-run flips ~5 lines and moves that
score by ~3 (batches run in parallel and share discovered names), so read
every changed line (diff) rather than trusting a small delta.

Runs go to ../data/attreval-runs/ (a live backend on :8080 is required;
LECTABLE_URL points it at another instance, e.g. a copy running a
candidate model).
Each run pauses the job queue and waits for it to drain first, so its
previews run alone (no model swaps against generation), then resumes it -
generation stops for the ~5-10 minutes a run takes.

Earlier chapters' reader corrections are in every prompt (the backend's
default); --no-corrections leaves them out, for an A/B in one build.
--given=half also shows the chapter's own corrected lines at even paragraph
idx with the reader's speaker, as a re-run after fixing some lines would
(those lines are then left out of every score). HELD-OUT HAND-CORRECTED
scores only the odd-idx corrected lines, so it compares across runs with
and without --given=half.

usage: attreval.py <label> [set] [--given=half] [--no-corrections]
       attreval.py summary <label>
       attreval.py errors <label> [book]
       attreval.py diff <labelA> <labelB>  FIXED/BROKE per line
       attreval.py gold <label>            freeze <label>'s "current" as gold.json
       attreval.py sq <label>              scare-quote pass vs sq_gold.json (+ controls)
"""
import json, os, sys, time, urllib.request
from concurrent.futures import ThreadPoolExecutor

HOST = os.environ.get("LECTABLE_URL", "http://localhost:8080").rstrip("/")
BASE = HOST + "/api/books"
ARISE = "b225960dc6812f3a8bfc534cb64e94b1"
SPIRE = "c228368c10437fa0e83b436a7042b375"
JAKE = "3fe07ab5323c39550cf03bbde77d4245"
SETS = {
    # (book, chapter idx, scored?) - Jake's is a drift check only.
    "full": [(ARISE, i, True) for i in (9, 10, 11, 12, 13, 15, 22, 25, 28, 31, 34)]
    + [(SPIRE, i, True) for i in (40, 44, 48, 52, 56)]
    + [(JAKE, i, False) for i in (10, 40, 70)],
}
NAMES = {ARISE: "arise", SPIRE: "spire", JAKE: "jake"}
HERE = os.path.dirname(os.path.abspath(__file__))
RUNS = os.path.join(HERE, "..", "data", "attreval-runs")


class exclusive:
    """Pause the job queue and wait for it to drain (POST
    /api/jobs/pause?drain=1) for the length of a run, so its previews run
    alone instead of swapping models against generation; resume after,
    unless the queue was already paused before."""

    def __enter__(self):
        with urllib.request.urlopen(HOST + "/api/jobs") as r:
            self.was_paused = json.load(r)["paused"]
        print("pausing the job queue and waiting for it to drain...", flush=True)
        urllib.request.urlopen(urllib.request.Request(HOST + "/api/jobs/pause?drain=1", method="POST"), timeout=3600)
        return self

    def __exit__(self, *exc):
        if not self.was_paused:
            urllib.request.urlopen(urllib.request.Request(HOST + "/api/jobs/resume", method="POST"))
            print("job queue resumed", flush=True)


def preview(book, idx, query=""):
    req = urllib.request.Request(f"{BASE}/{book}/chapters/{idx}/attribute-speakers/preview{query}", method="POST")
    t = time.time()
    with urllib.request.urlopen(req, timeout=1800) as r:
        d = json.load(r)
    d["seconds"] = round(time.time() - t, 1)
    return d


def run(label, set_name="full", given="none", corrections=True):
    os.makedirs(RUNS, exist_ok=True)
    chapters = SETS[set_name]
    params = [f"given={given}"] + ([] if corrections else ["corrections=0"])
    query = "?" + "&".join(params)
    # Two at a time: the worker has two LLM slots and a chapter's own
    # batches already fill both, so more just queues.
    with exclusive(), ThreadPoolExecutor(2) as ex:
        results = list(ex.map(lambda c: (c, preview(c[0], c[1], query)), chapters))
    out = []
    for (book, idx, scored), d in results:
        out.append({"book": NAMES[book], "chapter": idx, "scored": scored, "seconds": d["seconds"], "error": d.get("error"), "given": given, "corrections": corrections, "lines": d["lines"]})
    json.dump(out, open(os.path.join(RUNS, label + ".json"), "w"), indent=1)
    summarize(label)


GOLD_PATH = os.path.join(HERE, "gold.json")


def load_gold():
    """gold.json: the reviewed reference speakers for every scored line
    ({"book/chapter/paragraph": speaker}), written by `attreval.py gold
    <label>` from a fresh run's "current" values after the chapters were
    read through and corrected by hand (2026-09-29: all 19 chapters, 81
    lines fixed). Scoring uses it over a run's own "current" when present,
    so old runs rescore against the reviewed reference."""
    if os.path.exists(GOLD_PATH):
        return json.load(open(GOLD_PATH))
    return {}


GOLD = load_gold()


def gold_of(book, chapter, line):
    return GOLD.get(f"{book}/{chapter}/{line['paragraphIdx']}", line["current"])


ROLE_ALIASES_PATH = os.path.join(HERE, "role_aliases.json")


def load_role_aliases():
    """role_aliases.json: per book, groups of labels that name the same
    person ("Waiter"/"Server") - stored role labels are arbitrary enough
    that role-vs-role wording differences would otherwise count as errors.
    Returns {book: {lowercased label: canonical label}}."""
    if not os.path.exists(ROLE_ALIASES_PATH):
        return {}
    out = {}
    for book, groups in json.load(open(ROLE_ALIASES_PATH)).items():
        if book.startswith("_"):
            continue
        out[book] = {name.lower(): group[0] for group in groups for name in group}
    return out


ROLE_ALIASES = load_role_aliases()


def same(book, a, b, strict=False):
    """Whether speakers a and b match: exactly, or (unless strict) case-
    insensitively or through role_aliases.json."""
    if a == b or strict:
        return a == b
    aliases = ROLE_ALIASES.get(book, {})
    return aliases.get(a.lower(), a.lower()) == aliases.get(b.lower(), b.lower())


def correct(ch, l, strict=False):
    return same(ch["book"], l["proposed"], gold_of(ch["book"], ch["chapter"], l), strict)


def scored_lines(ch):
    """A chapter's lines minus any the run was given (?given=)."""
    return [l for l in ch["lines"] if not l.get("given")]


def score(ch, strict=False):
    lines = scored_lines(ch)
    ok = sum(1 for l in lines if correct(ch, l, strict))
    return ok, len(lines)


def write_gold(label):
    data = json.load(open(os.path.join(RUNS, label + ".json")))
    gold = {f"{c['book']}/{c['chapter']}/{l['paragraphIdx']}": l["current"] for c in data for l in c["lines"]}
    json.dump(gold, open(GOLD_PATH, "w"), indent=0, sort_keys=True)
    print(f"wrote {len(gold)} reference lines")


def hand_edited():
    """(book, chapter, paragraph) the reader corrected by hand - certain
    gold: from the server log (edits before paragraph_edits existed) plus
    GET .../paragraph-edits."""
    out = set()
    path = os.path.join(HERE, "arise_edited.txt")
    if os.path.exists(path):
        for line in open(path):
            ch, idx = line.split()
            out.add(("arise", int(ch), int(idx)))
    for book, name in NAMES.items():
        with urllib.request.urlopen(f"{BASE}/{book}/paragraph-edits?field=speaker") as r:
            for e in json.load(r)["edits"]:
                out.add((name, e["chapterIdx"], e["paragraphIdx"]))
    return out


def summarize(label):
    data = json.load(open(os.path.join(RUNS, label + ".json")))
    tot = {}
    for ch in data:
        ok, n = score(ch)
        strict_ok, _ = score(ch, strict=True)
        key = ch["book"]
        a = tot.setdefault(key, [0, 0, 0])
        a[0] += ok
        a[1] += n
        a[2] += strict_ok
        tag = "" if ch["scored"] else " (drift)"
        print(f"{ch['book']:6} ch{ch['chapter']:<4} {ok:4}/{n:<4} {100*ok/max(n,1):5.1f}%  {ch['seconds']}s{tag}{'  ERR ' + ch['error'] if ch['error'] else ''}")
    for k, (ok, n, strict_ok) in tot.items():
        aliased = f"  (strict {strict_ok}, +{ok - strict_ok} via role_aliases.json)" if ok != strict_ok else ""
        print(f"TOTAL {k:6} {ok}/{n} {100*ok/max(n,1):.1f}%{aliased}")
    edited = hand_edited()
    hits = [(c, l) for c in data for l in scored_lines(c) if (c["book"], c["chapter"], l["paragraphIdx"]) in edited]
    ok = sum(1 for c, l in hits if correct(c, l))
    print(f"HAND-CORRECTED LINES {ok}/{len(hits)}")
    held = [(c, l) for c, l in hits if l["paragraphIdx"] % 2]
    ok = sum(1 for c, l in held if correct(c, l))
    print(f"HELD-OUT HAND-CORRECTED (odd ¶) {ok}/{len(held)}")
    role_mismatches(data)


def role_mismatches(data):
    """Lists errors where neither side is a named (non-role) character or
    a sentinel - usually two wordings of one role ("Girl"/"Bottle Blonde"),
    which belong in role_aliases.json once checked against the text, but
    sometimes two different unnamed people, which are real errors."""
    named = {}
    for book, name in NAMES.items():
        with urllib.request.urlopen(f"{BASE}/{book}/speakers") as r:
            named[name] = {s["name"].lower() for s in json.load(r) if not s.get("role")}
    pairs = {}
    for ch in data:
        if not ch["scored"]:
            continue
        for l in scored_lines(ch):
            if correct(ch, l):
                continue
            g, p = gold_of(ch["book"], ch["chapter"], l), l["proposed"]
            if any(x in ("Unknown", "Narrator") or x.lower() in named[ch["book"]] for x in (g, p)):
                continue
            pairs.setdefault((ch["book"], g, p), []).append(f"ch{ch['chapter']} ¶{l['paragraphIdx']}")
    if pairs:
        print("ROLE-VS-ROLE ERRORS (add to role_aliases.json only if the text shows one person):")
        for (book, g, p), where in sorted(pairs.items()):
            print(f"  {book}: gold={g} got={p}  {', '.join(where)}")


def errors(label, book=None):
    data = json.load(open(os.path.join(RUNS, label + ".json")))
    for ch in data:
        if book and ch["book"] != book:
            continue
        for l in scored_lines(ch):
            g = gold_of(ch["book"], ch["chapter"], l)
            if not correct(ch, l):
                print(f"{ch['book']} ch{ch['chapter']} ¶{l['paragraphIdx']}: gold={g} got={l['proposed']} | {l['text'][:90]}")


def diff(a, b):
    A = {(c["book"], c["chapter"]): {l["paragraphIdx"]: l for l in scored_lines(c)} for c in json.load(open(os.path.join(RUNS, a + ".json")))}
    B = {(c["book"], c["chapter"]): {l["paragraphIdx"]: l for l in scored_lines(c)} for c in json.load(open(os.path.join(RUNS, b + ".json")))}
    fixed = broke = 0
    for key in A:
        for idx, la in A[key].items():
            lb = B.get(key, {}).get(idx)
            if not lb or same(key[0], la["proposed"], lb["proposed"]):
                continue
            gold = gold_of(key[0], key[1], la)
            mark = "FIXED" if same(key[0], lb["proposed"], gold) else ("BROKE" if same(key[0], la["proposed"], gold) else "CHANGED")
            fixed += mark == "FIXED"
            broke += mark == "BROKE"
            print(f"{mark:7} {key[0]} ch{key[1]} ¶{idx}: gold={gold} {la['proposed']} -> {lb['proposed']} | {la['text'][:70]}")
    print(f"fixed {fixed}, broke {broke}")


SQ_GOLD_PATH = os.path.join(HERE, "sq_gold.json")
# Scare-quote controls: chapters of other books, scored only by reading any
# flag a run adds (they have no reviewed reference).
SQ_CONTROLS = [(SPIRE, i) for i in (40, 44, 48, 52, 56, 60, 80, 100)] + [(JAKE, i) for i in (10, 40, 70)]


def sq_preview(book, idx):
    req = urllib.request.Request(f"{BASE}/{book}/chapters/{idx}/retag-scare-quotes/preview", method="POST")
    with urllib.request.urlopen(req, timeout=1800) as r:
        return json.load(r)


def sq_run(label):
    """Scare-quote eval: the pass's preview over every Arise chapter in
    sq_gold.json (candidates in ch. 0-37, read through by hand: 112 scare
    quotes, 8 embedded real speech) plus SQ_CONTROLS."""
    gold = json.load(open(SQ_GOLD_PATH))
    chapters = sorted({int(k.split("/")[1]) for k in gold})
    jobs_ = [(ARISE, ch) for ch in chapters] + SQ_CONTROLS
    with exclusive(), ThreadPoolExecutor(2) as ex:
        results = list(ex.map(lambda c: (c, sq_preview(*c)), jobs_))
    out = [{"book": NAMES[b], "chapter": ch, "error": d.get("error"), "lines": d["lines"]} for (b, ch), d in results]
    os.makedirs(RUNS, exist_ok=True)
    json.dump(out, open(os.path.join(RUNS, "sq-" + label + ".json"), "w"), indent=1)
    sq_summary(label)


def sq_summary(label):
    gold = json.load(open(SQ_GOLD_PATH))
    data = json.load(open(os.path.join(RUNS, "sq-" + label + ".json")))
    proposed = {f"{c['book']}/{c['chapter']}/{l['paragraphIdx']}": l for c in data for l in c["lines"]}
    tp = [k for k, v in gold.items() if v and proposed.get(k, {}).get("proposed")]
    fn = [k for k, v in gold.items() if v and not proposed.get(k, {}).get("proposed")]
    fp = [k for k, v in gold.items() if not v and proposed.get(k, {}).get("proposed")]
    extra = [k for k, l in proposed.items() if k.startswith("arise/") and k not in gold and l["proposed"]]
    for c in data:
        if c["error"]:
            print("ERR", c["book"], c["chapter"], c["error"])
    print(f"ARISE recall {len(tp)}/{len(tp)+len(fn)}  false positives {len(fp)}/{sum(1 for v in gold.values() if not v)} (+{len(extra)} unreviewed flags)")
    for k in fn:
        print("  missed", k, proposed.get(k, {}).get("text", "(not asked)")[:70])
    for k in fp + extra:
        print("  flagged", k, proposed[k]["text"][:70])
    for c in data:
        if c["book"] == "arise":
            continue
        for l in c["lines"]:
            if l["proposed"] and not l["current"]:
                print(f"  CONTROL new flag {c['book']} ch{c['chapter']} ¶{l['paragraphIdx']}: {l['text'][:80]}")
            if l["current"] and not l["proposed"]:
                print(f"  CONTROL dropped {c['book']} ch{c['chapter']} ¶{l['paragraphIdx']}: {l['text'][:80]}")


if __name__ == "__main__":
    if sys.argv[1] == "sq":
        sq_run(sys.argv[2])
        sys.exit()
    if sys.argv[1] == "sq-summary":
        sq_summary(sys.argv[2])
        sys.exit()
    if sys.argv[1] == "diff":
        diff(sys.argv[2], sys.argv[3])
    elif sys.argv[1] == "errors":
        errors(sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else None)
    elif sys.argv[1] == "gold":
        write_gold(sys.argv[2])
    elif sys.argv[1] == "summary":
        summarize(sys.argv[2])
    else:
        flags = [a for a in sys.argv[2:] if a.startswith("--")]
        rest = [a for a in sys.argv[2:] if not a.startswith("--")]
        given = next((f.split("=", 1)[1] for f in flags if f.startswith("--given=")), "none")
        run(sys.argv[1], rest[0] if rest else "full", given=given, corrections="--no-corrections" not in flags)
