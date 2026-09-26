import struct,sys,collections,re
# bytes per element-block for ggml types: (block_size, type_size)
BS={0:(1,4),1:(1,2),30:(1,2),8:(32,34),2:(32,18),3:(32,20),6:(32,22),7:(32,24),10:(256,84),11:(256,110),12:(256,144),13:(256,176),14:(256,210)}
TN={0:'f32',1:'f16',30:'bf16',8:'q8_0',12:'q4_k',13:'q5_k',14:'q6_k'}
def rd(f,fmt): return struct.unpack('<'+fmt,f.read(struct.calcsize('<'+fmt)))
def rs(f): (n,)=rd(f,'Q'); return f.read(n).decode('utf8','replace')
SZ={0:'B',1:'b',2:'H',3:'h',4:'I',5:'i',6:'f',7:'?',10:'Q',11:'q',12:'d'}
def skip(f,t):
    if t==8: rs(f)
    elif t==9:
        (at,)=rd(f,'I');(n,)=rd(f,'Q')
        if at==8:
            for _ in range(n): rs(f)
        else: f.seek(n*struct.calcsize('<'+SZ[at]),1)
    else: rd(f,SZ[t])
f=open(sys.argv[1],'rb'); assert f.read(4)==b'GGUF'
v,nt,nkv=rd(f,'IQQ')
for _ in range(nkv): rs(f); (t,)=rd(f,'I'); skip(f,t)
depth=int(sys.argv[2]) if len(sys.argv)>2 else 2
agg=collections.defaultdict(lambda:[0,0,collections.Counter()])
for _ in range(nt):
    name=rs(f); (nd,)=rd(f,'I'); dims=rd(f,'Q'*nd); (t,)=rd(f,'I'); rd(f,'Q')
    n=1
    for d in dims: n*=d
    b,s=BS[t]; by=n//b*s
    key='.'.join(re.split(r'[./]',name)[:depth])
    a=agg[key]; a[0]+=by; a[1]+=n; a[2][TN.get(t,t)]+=1
tot=sum(a[0] for a in agg.values())
for k,a in sorted(agg.items(),key=lambda x:-x[1][0]):
    print(f"{a[0]/2**20:9.1f} MiB  {a[1]/1e6:8.1f} M params  {k}  {dict(a[2])}")
print(f"{tot/2**20:9.1f} MiB total")
