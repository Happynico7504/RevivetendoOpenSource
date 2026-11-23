import sys, pickle, os, bisect, collections
sys.path.insert(0, os.path.dirname(__file__))
from ppc import Bin, dis

class Ctx:
    def __init__(self, path):
        self.b = Bin(path)
        cache = path + '.xref.pkl'
        if os.path.exists(cache):
            self.xr, self.calls, self.funcs = pickle.load(open(cache, 'rb'))
        else:
            self.build(); pickle.dump((self.xr, self.calls, self.funcs), open(cache, 'wb'))
        self.fsorted = sorted(self.funcs)

    def build(self):
        b = self.b
        xr = collections.defaultdict(list)  # data target -> insn addrs
        calls = collections.defaultdict(list)  # call target -> callers
        funcs = set()
        for off, (t, tgt) in b.rel.items():
            if b.text_addr <= off < b.text_addr + b.text_size:
                if t in (4, 6, 5):  # LO / HA / HI
                    if t == 4: xr[tgt].append(off - 2)
                elif t == 10:
                    calls[tgt].append(off); funcs.add(tgt)
            else:
                # data pointers (vtables, tables)
                if t == 1: xr[tgt].append(off)
        ws = b.words()
        for i, w in enumerate(ws):
            if (w >> 26) == 18 and (w & 1):
                pc = b.text_addr + i*4
                m, tgt = dis(w, pc)
                if pc not in b.rel:
                    calls[tgt].append(pc); funcs.add(tgt)
            if w & 0xffff0000 == 0x94210000 and w & 0x8000:  # stwu r1,-x(r1)
                funcs.add(b.text_addr + i*4)
        self.xr, self.calls, self.funcs = dict(xr), dict(calls), funcs

    def func_of(self, a):
        i = bisect.bisect_right(self.fsorted, a) - 1
        return self.fsorted[i] if i >= 0 else None

    def name(self, a):
        if a in self.b.syms: return self.b.syms[a]
        return 'sub_%08x' % a

    def disasm(self, start, end=None, maxn=4000):
        b = self.b; out = []
        a = start; n = 0
        while n < maxn:
            w = b.u32(a); m, tgt = dis(w, a)
            ann = ''
            r = b.rel.get(a + 2) or b.rel.get(a)
            if r and r[0] in (4, 6, 5, 1):
                s = b.cstr(r[1]) if 0x10000000 <= r[1] < 0x10200000 else None
                ann = ' ; -> %08x' % r[1] + (' "%s"' % s[:80] if s and len(s) >= 2 and s.isprintable() else '')
            if m in ('bl', 'b') and tgt is not None:
                rr = b.rel.get(a)
                if rr and rr[0] == 10: tgt = rr[1]
                m = '%s %s' % (m, self.name(tgt))
            elif tgt is not None: m = '%s 0x%08x' % (m, tgt)
            out.append('%08x: %s%s' % (a, m, ann))
            a += 4; n += 1
            if end is None and (m == 'blr') and n > 4:
                # stop at blr if next looks like func start
                nw = b.u32(a)
                if a in self.funcs or (nw & 0xffff8000 == 0x94218000): break
            if end is not None and a >= end: break
        return '\n'.join(out)

if __name__ == '__main__':
    c = Ctx(sys.argv[1])
    print(len(c.xr), len(c.calls), len(c.funcs))
