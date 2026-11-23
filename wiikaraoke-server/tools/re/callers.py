import sys;sys.path.insert(0,__import__('os').path.dirname(__file__))
from xref import Ctx
from ppc import dis
c=Ctx('app.elf');b=c.b
ws=b.words()
_br={}
for i,w in enumerate(ws):
    if w>>26==18:
        pc=b.text_addr+i*4; m,t=dis(w,pc); _br.setdefault(t,[]).append(pc)
def callers(f):
    r=set(_br.get(f,[]))|set(c.calls.get(f,[]))
    return sorted(r)
if __name__=='__main__':
    for a in sys.argv[1:]:
        f=int(a,16); print(a,[(hex(x),hex(c.func_of(x))) for x in callers(f)])
