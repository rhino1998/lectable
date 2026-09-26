import json,sys,collections
rows=[json.loads(l) for l in open(sys.argv[1])]
# last result wins per (codec,kind,file)
d={}
for r in rows: d[(r['Codec'],r['Kind'],r['File'])]=r
agg=collections.defaultdict(lambda: dict(pcm=0,size=0,sec=0,enc=0,dec=0,rss=0,ok=0,n=0))
for r in d.values():
    a=agg[(r['Kind'],r['Codec'])]; a['n']+=1; a['ok']+=r['OK']
    a['pcm']+=r['PCMBytes']; a['size']+=r['Size']; a['sec']+=r['Seconds']
    a['enc']+=r['EncCPU']; a['dec']+=r['DecCPU']; a['rss']=max(a['rss'],r['DecRSSKB'])
for kind in ['voice','variant','seed','ambience']:
    ks=sorted([k for k in agg if k[0]==kind],key=lambda k: agg[k]['size']/max(1,agg[k]['pcm']))
    if not ks: continue
    flac=agg.get((kind,'flac8'))
    print(f"\n## {kind}\n| codec | size % PCM | vs flac8 | enc xRT | dec xRT | dec peak RSS MB | verified |\n|---|---|---|---|---|---|---|")
    for k in ks:
        a=agg[k]; p=100*a['size']/a['pcm']
        vs=f"{100*(a['size']/flac['size']-1):+.1f}%" if flac else ''
        print(f"| {k[1]} | {p:.2f}% | {vs} | {a['sec']/max(a['enc'],1e-9):.0f} | {a['sec']/max(a['dec'],1e-9):.0f} | {a['rss']/1024:.0f} | {a['ok']}/{a['n']} |")
