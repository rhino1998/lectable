#!/usr/bin/env python3
"""First-pass attribution eval: how a book comes out of preprocessing with
no human in the loop, scored against the same book's corrected speakers.

attreval.py's preview runs attribution against the library as it stands -
the roster after review, the reader's corrections, summaries written in an
earlier pass - which a book being preprocessed for the first time never
has. This uploads a fresh copy of a book's source epub as a series of its
own (so its roster starts empty, as a series' first book's does), runs the
real preprocessing pipeline on it with every phase that doesn't feed
attribution cancelled, and compares each dialogue line's stored speaker
with the original book's.

How the server preprocesses is set by its environment, so each config is a
backend restart (`make deploy-backend` with these exported) and a run:

  default               summaries for the whole book, then attribution
  LECTABLE_PREPROCESS_CHAIN=1
                        summary then attribution, a chapter at a time
                        (httpapi.preprocessChainPhase)
  LECTABLE_SPEAKER_ATTR_DECIDE=1
                        attribution by choice (speakerattr.AttributeOptions)

A restart loses the queue, so run it when nothing else is queued. Unlike
attreval.py it doesn't pause the queue: preprocessing runs as ordinary
jobs - though the LLM and narration take turns on the GPU, so it still
holds up any rendering going on.

Two scores per run, over the original's dialogue lines that have a speaker:
  exact     the copy gave the line the original's name
  grouped   the copy's names mapped each to the original name it most often
            stands for - lines put with the right person under another
            name ("Dwarf" for Oren), which a roster review can still fix

usage: firstpass.py run <label> <book id> [--keep]
       firstpass.py score <label> <copy id> <book id>

run uploads the copy (series "eval <label>"), preprocesses it, scores it
and deletes it again unless --keep. Results go to
../data/attreval-runs/firstpass-<label>.json.
"""
import difflib, io, json, os, re, sys, time, urllib.error, urllib.parse, urllib.request, uuid, zipfile
from collections import Counter, defaultdict

HOST = os.environ.get("LECTABLE_URL", "http://localhost:8080").rstrip("/")
API = HOST + "/api"
HERE = os.path.dirname(os.path.abspath(__file__))
DATA = os.path.join(HERE, "..", "data")
RUNS = os.path.join(DATA, "attreval-runs")

# Phases and tasks of the copy that don't feed attribution: cancelled as
# they appear. Scare quotes, the matter pass, the setting profile and the
# summaries all do, and run.
CANCEL = {
    "pipeline_characterization", "pipeline_voice_provision", "pipeline_direction",
    "pipeline_ambience", "pipeline_pronunciation", "pipeline_embedding",
    "voice_clone", "voice_design", "voice_provision", "speaker_characterization",
    "speech_direction", "ambience", "chapter_embedding",
}


def call(method, path, body=None, headers=None):
    req = urllib.request.Request(API + path, data=body, method=method, headers=headers or {})
    with urllib.request.urlopen(req, timeout=600) as r:
        raw = r.read()
    return json.loads(raw) if raw else None


def get(path):
    return call("GET", path)


def copy_epub(book_id, series):
    """The book's source epub with its series metadata replaced."""
    src = os.path.join(DATA, "epubs", book_id + ".epub")
    out = io.BytesIO()
    with zipfile.ZipFile(src) as zin, zipfile.ZipFile(out, "w") as zout:
        for item in zin.infolist():
            data = zin.read(item.filename)
            if item.filename.endswith(".opf"):
                text = data.decode("utf-8")
                text, n = re.subn(r'(<meta name="calibre:series" content=")[^"]*(")', lambda m: m.group(1) + series + m.group(2), text)
                if n == 0:
                    text = text.replace("</metadata>", f'<meta name="calibre:series" content="{series}"/>\n</metadata>')
                data = text.encode("utf-8")
            # mimetype stays first and stored, as the format requires.
            zout.writestr(item, data, compress_type=zipfile.ZIP_STORED if item.filename == "mimetype" else zipfile.ZIP_DEFLATED)
    return out.getvalue()


def upload(name, data):
    boundary = uuid.uuid4().hex
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{name}\"\r\n"
            "Content-Type: application/epub+zip\r\n\r\n").encode() + data + f"\r\n--{boundary}--\r\n".encode()
    return call("POST", "/books", body, {"Content-Type": f"multipart/form-data; boundary={boundary}"})


def book_tasks(book_id):
    d = get(f"/jobs?bookId={book_id}")
    return (d.get("inFlight") or []) + (d.get("queued") or [])


def preprocess(copy_id):
    """Runs the copy's preprocessing to the end, cancelling CANCEL tasks as
    they appear. Returns the wall seconds it took."""
    call("POST", f"/books/{copy_id}/preprocess")
    start, quiet = time.time(), 0
    while True:
        tasks = book_tasks(copy_id)
        for t in tasks:
            if t["kind"] in CANCEL:
                try:
                    call("DELETE", f"/jobs/{urllib.parse.quote(t['id'], safe='')}")
                except urllib.error.HTTPError:
                    pass  # finished meanwhile
        busy = [t for t in tasks if t["kind"] not in CANCEL]
        preprocessing = get(f"/books/{copy_id}")["preprocessing"]
        quiet = 0 if busy or preprocessing else quiet + 1
        if quiet >= 6:  # nothing for 30s
            return time.time() - start
        if int(time.time() - start) % 60 < 5:
            print(f"  {time.time() - start:5.0f}s: {len(busy)} tasks: {dict(Counter(t['kind'] for t in busy))}", flush=True)
        time.sleep(5)


def chapters(book_id):
    n = len(get(f"/books/{book_id}")["chapters"])
    return [get(f"/books/{book_id}/chapters/{i}") for i in range(n)]


def fold(name):
    return name.strip().lower().replace("’", "'")


def score(label, copy_id, book_id):
    gold_chs, copy_chs = chapters(book_id), chapters(copy_id)
    if [c["title"] for c in gold_chs] != [c["title"] for c in copy_chs]:
        print("WARNING: chapter titles differ between the copy and the original")
    pairs = []  # (chapter idx, paragraph idx, text, gold, got)
    for g, c in zip(gold_chs, copy_chs):
        gp, cp = g["paragraphs"], c["paragraphs"]
        sm = difflib.SequenceMatcher(None, [p["text"] for p in gp], [p["text"] for p in cp], autojunk=False)
        for a, b, n in sm.get_matching_blocks():
            for k in range(n):
                gpar, cpar = gp[a + k], cp[b + k]
                if gpar.get("isQuote") and gpar.get("speaker"):
                    pairs.append((g["idx"], gpar["idx"], gpar["text"], gpar["speaker"], cpar.get("speaker") or ""))
    exact = sum(1 for p in pairs if fold(p[3]) == fold(p[4]))
    # Each copy name stands for the gold name it most often coincides with.
    by_got = defaultdict(Counter)
    for p in pairs:
        by_got[fold(p[4])][fold(p[3])] += 1
    stands_for = {got: c.most_common(1)[0][0] for got, c in by_got.items()}
    grouped = sum(1 for p in pairs if stands_for[fold(p[4])] == fold(p[3]) and fold(p[4]) not in ("", "unknown", "narrator"))
    n = len(pairs)
    print(f"{label}: {n} lines, exact {exact} ({100*exact/max(n,1):.1f}%), grouped {grouped} ({100*grouped/max(n,1):.1f}%), "
          f"{len({fold(p[4]) for p in pairs})} names given for {len({fold(p[3]) for p in pairs})}")
    per = defaultdict(lambda: [0, 0])
    for p in pairs:
        per[p[0]][0] += fold(p[3]) == fold(p[4])
        per[p[0]][1] += 1
    for idx in sorted(per):
        ok, tot = per[idx]
        print(f"  ch{idx:<4} {ok:4}/{tot:<4} {100*ok/tot:5.1f}%")
    os.makedirs(RUNS, exist_ok=True)
    json.dump({"label": label, "copy": copy_id, "book": book_id, "exact": exact, "grouped": grouped, "lines": n,
               "pairs": [{"chapter": p[0], "paragraphIdx": p[1], "text": p[2], "gold": p[3], "got": p[4]} for p in pairs]},
              open(os.path.join(RUNS, f"firstpass-{label}.json"), "w"), indent=1)


def run(label, book_id, keep):
    if book_tasks(book_id):
        print("the original book has queued work; its speakers may still change - wait for it first")
        return
    series = f"eval {label}"
    copy = upload(f"{label}.epub", copy_epub(book_id, series))
    copy_id = copy["id"]
    print(f"copy {copy_id} in series {series!r}", flush=True)
    try:
        seconds = preprocess(copy_id)
        print(f"preprocessed in {seconds:.0f}s")
        score(label, copy_id, book_id)
    finally:
        if not keep:
            call("DELETE", f"/books/{copy_id}")
            print(f"deleted copy {copy_id}")


if __name__ == "__main__":
    if len(sys.argv) >= 4 and sys.argv[1] == "run":
        run(sys.argv[2], sys.argv[3], "--keep" in sys.argv)
    elif len(sys.argv) == 5 and sys.argv[1] == "score":
        score(sys.argv[2], sys.argv[3], sys.argv[4])
    else:
        print(__doc__)
        sys.exit(2)
