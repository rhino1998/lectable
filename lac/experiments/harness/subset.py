import json,sys,collections
n=int(sys.argv[2]) if len(sys.argv)>2 else 20
want={f"{i*100//n:03d}.wav" for i in range(n)}
d={}
for l in open(sys.argv[1]):
    r=json.loads(l); d[(r['Codec'],r['Kind'],r['File'])]=r
agg=collections.defaultdict(lambda:[0,0])
for (c,k,f),r in d.items():
    if f in want: agg[(k,c)][0]+=r['Size']; agg[(k,c)][1]+=r['PCMBytes']
for k in ['voice','variant','seed','ambience']:
    print(k, '  '.join(f"{c}={100*s/p:.2f}" for (kk,c),(s,p) in sorted(agg.items(), key=lambda x:x[1][0]/x[1][1]) if kk==k))
