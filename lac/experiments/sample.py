import glob, random, os, numpy as np, scipy.io.wavfile as wf, shutil, collections
D='/home/rhino/lectable/backend/data'
OUT=os.path.join(os.path.dirname(__file__),'samples')
rng=random.Random(20260926)
def rms(p):
    sr,x=wf.read(p); x=x.astype(np.float64); return float(np.sqrt((x*x).mean())+1e-9)
def strat(files,n,key=rms):
    # sort by RMS, take one random pick from each of n equal-count strata
    v=sorted(files,key=key); out=[]
    for i in range(n):
        a=len(v)*i//n; b=len(v)*(i+1)//n; out.append(rng.choice(v[a:b]))
    return out
base=sorted(glob.glob(D+'/voice-refs/*.wav'))
var=sorted(glob.glob(D+'/voice-refs/variants/*/*.wav'))
seeds=sorted(glob.glob(D+'/audio/c228368c10437fa0e83b436a7042b375/*/music/*.seed.wav'))
amb=sorted(glob.glob(D+'/audio/c228368c10437fa0e83b436a7042b375/ambience/*.wav'))
print(len(base),len(var),len(seeds),len(amb))
sel={}
sel['voice']=strat(base,100)
# variants: stratify by emotion (proportional, >=3 each) then RMS within
by=collections.defaultdict(list)
for p in var: by[os.path.basename(p)].append(p)
sv=[]
for e,ps in sorted(by.items()):
    k=max(3,round(100*len(ps)/len(var))); sv+=strat(ps,min(k,len(ps)))
sel['variant']=sv[:100] if len(sv)>=100 else sv
sel['seed']=strat(seeds,100)
sel['ambience']=strat(amb,100)
for k,ps in sel.items():
    os.makedirs(f'{OUT}/{k}',exist_ok=True)
    with open(f'{OUT}/{k}.list','w') as f:
        for i,p in enumerate(ps):
            f.write(os.path.relpath(p,D)+'\n'); shutil.copy(p,f'{OUT}/{k}/{i:03d}.wav')
    print(k,len(ps))
