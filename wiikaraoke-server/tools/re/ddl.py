import struct, sys
class R:
    def __init__(s,d,p): s.d=d; s.p=p
    def u8(s): v=s.d[s.p]; s.p+=1; return v
    def u32(s): v=struct.unpack_from('>I',s.d,s.p)[0]; s.p+=4; return v
    def str(s): n=s.u32(); v=s.d[s.p:s.p+n].decode('latin1'); s.p+=n; return v
def nsitem(r): a=r.str(); b=r.str(); return a
def decluse(r):
    t=r.u8(); name=r.str()
    if t==18:
        base=r.str(); n=r.u8(); args=[decluse(r) for _ in range(n)]
        return '%s<%s>'%(base,','.join(args))
    return name
def decl(r):
    name=nsitem(r); unit=r.str(); props=namespace(r); return name
def variable(r): name=nsitem(r); t=decluse(r); arr=r.u32(); return (name,t,arr)
def methoddecl(r): name=decl(r); params=namespace(r); return name,params
def element(r):
    t=r.u8()
    if t==1: return ('item',nsitem(r))
    if t==2: return ('decl',decl(r))
    if t==3: n=decl(r); parent=r.str(); cid=r.u32(); ns=namespace(r); return ('doclass',n,parent,ns)
    if t==4: n=decl(r); ns=namespace(r); return ('dataset',n,ns)
    if t==5: return ('type',decl(r))
    if t==6: return ('var',)+variable(r)
    if t in (8,9): n,p=methoddecl(r); ps=namespace(r); return ('rmc' if t==8 else 'action',n,ps)
    if t==10: return ('adapter',decl(r))
    if t==11: n=decl(r); r.u32(); r.u32(); return ('prop',n)
    if t==12: n=decl(r); ms=namespace(r); return ('protocol',n,ms)
    if t==13: v=variable(r); t2=decluse(r); arr=r.u32(); io=r.u8(); return ('param',v[0],t2,io)
    if t==14: v=variable(r); t2=decluse(r); arr=r.u32(); return ('ret',v[0],t2)
    if t==15: n=decl(r); parent=r.str(); ns=namespace(r); return ('class',n,parent,ns)
    if t==16: n=decl(r); c=r.u32(); return ('template',n,c)
    if t==17: return ('simple',decl(r))
    if t==18: n=decl(r); base=r.str(); c=r.u32(); args=[r.str() for _ in range(c)]; return ('tinst',n,base,args)
    if t==19: n=decl(r); u=r.str(); dd=r.str(); return ('unit',n)
    if t==20: return ('dup',decl(r))
    raise ValueError('bad elem %d at %x'%(t,r.p))
def namespace(r):
    n=r.u32(); return [element(r) for _ in range(n)]
def dump(el,ind=0):
    pad='  '*ind
    k=el[0]
    if k=='protocol':
        print(pad+'protocol',el[1])
        for i,m in enumerate([m for m in el[2] if m[0] in('rmc','action')],1):
            ps=', '.join(('%s%s %s'%('out ' if p[0]=='param' and p[3]==2 else ('ret ' if p[0]=='ret' else ''),p[2],p[1])) for p in m[2])
            print(pad+'  %d %s(%s)'%(i,m[1],ps))
    elif k=='class':
        print(pad+'class %s : %s'%(el[1],el[2]))
        for v in el[3]:
            if v[0]=='var': print(pad+'   %s %s'%(v[2],v[1]))
    elif k in('doclass','dataset'): pass
d=open(sys.argv[1],'rb').read()
p=0
while True:
    p=d.find(bytes.fromhex('CD652312'),p)
    if p<0: break
    r=R(d,p+21)
    try:
        root=namespace(r)
        for el in root: dump(el)
    except Exception as e: print('ERR',e)
    p+=4
