import sys;sys.path.insert(0,__import__('os').path.dirname(__file__))
from xref import Ctx
c=Ctx('app.elf')
a=int(sys.argv[1],16)
e=int(sys.argv[2],16) if len(sys.argv)>2 else None
if e is None:
    f=c.func_of(a); print(c.disasm(f))
else: print(c.disasm(a,e))
