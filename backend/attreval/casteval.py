#!/usr/bin/env python3
"""Preview characterizations without storing them.

For each character named on the command line (most lines first when a name
exists in several series), calls POST .../characterize/preview, which casts
the voice but saves nothing, and prints it beside the stored casting
(--temp and --no-lookup vary the casting call). With --out, also writes the results as JSON.

    attreval/casteval.py Fritz Bert Sid
    attreval/casteval.py --top 24 --out /tmp/cast.json

Checks printed per casting: word count (the prompt asks for 35-60), a gender
word matching the stored pronouns, and visual or emotional-range leakage.
"""
import argparse
import json
import re
import sqlite3
import urllib.request

ap = argparse.ArgumentParser()
ap.add_argument("names", nargs="*")
ap.add_argument("--top", type=int, default=0, help="also the N characters with the most lines")
ap.add_argument("--db", default="data/library.sqlite")
ap.add_argument("--api", default="http://localhost:8080")
ap.add_argument("--temp", help="sampling temperature for the casting call (default: the backend's)")
ap.add_argument("--no-lookup", action="store_true", help="cast without the book lookups")
ap.add_argument("--quiet", action="store_true", help="print only the new casting")
ap.add_argument("--out")
args = ap.parse_args()

db = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
LINES = """
    select ch.id, ch.name, ch.pronouns, min(b.id), count(*) n
    from characters ch
    join books b on (case when b.series_name <> '' then 'series:' || b.series_name else 'book:' || b.id end) = ch.scope
    join chapters c on c.book_id = b.id
    join paragraphs p on p.chapter_id = c.id and p.speaker = ch.name
    where not ch.invalid {where}
    group by ch.id order by n desc {limit}"""
chars = []
for name in args.names:
    row = db.execute(LINES.format(where="and ch.name = ?", limit="limit 1"), (name,)).fetchone()
    if row is None:
        print(f"no character named {name!r} with lines")
        continue
    chars.append(row)
if args.top:
    seen = {c[0] for c in chars}
    chars += [r for r in db.execute(LINES.format(where="and not ch.is_role", limit=f"limit {args.top}")) if r[0] not in seen]

GENDER = {"he/him": r"\b(man|boy|male)\b", "she/her": r"\b(woman|girl|female)\b"}
VISUAL = r"\b(eyes?|hair|posture|scars?|clothes|build|stocky|gaunt)\b"
RANGE = r"\bwhen (angry|afraid|excited|pleading|upset)\b"


def checks(instruct, pronouns):
    words = len(instruct.split())
    flags = [f"{words}w"]
    if pronouns in GENDER and not re.search(GENDER[pronouns], instruct, re.I):
        flags.append("NO-GENDER")
    if re.search(VISUAL, instruct, re.I):
        flags.append("VISUAL")
    if re.search(RANGE, instruct, re.I):
        flags.append("RANGE")
    return " ".join(flags)


results = []
for cid, name, pronouns, book, n in chars:
    params = [p for p in (f"temp={args.temp}" if args.temp else "", "lookup=0" if args.no_lookup else "") if p]
    url = f"{args.api}/api/books/{book}/characters/{cid}/characterize/preview" + ("?" + "&".join(params) if params else "")
    req = urllib.request.Request(url, method="POST")
    try:
        res = json.load(urllib.request.urlopen(req, timeout=600))
    except Exception as e:  # noqa: BLE001
        print(f"=== {name}: {e}")
        continue
    results.append({"name": name, "pronouns": pronouns, "lines": n, **res})
    print(f"=== {name} [{pronouns}] {n} lines")
    for lookup in res.get("lookups") or []:
        print(f"  lookup    {lookup}")
    print(f"  NEW [{checks(res['summary'], pronouns)}] {res['summary']}")
    if args.quiet:
        continue
    print(f"  OLD [{checks(res['currentSummary'], pronouns)}] {res['currentSummary']}")
    print(f"  ref {res['refLine']}", flush=True)

if args.out:
    json.dump(results, open(args.out, "w"), indent=1)
