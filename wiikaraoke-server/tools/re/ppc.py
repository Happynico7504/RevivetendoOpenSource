"""Minimal PowerPC (Espresso) decoder + RPX helpers for xref / disassembly work."""
import struct, bisect, re

def s16(x): return x - 0x10000 if x & 0x8000 else x

class Bin:
    def __init__(self, path):
        d = open(path, 'rb').read()
        self.d = d
        shoff = struct.unpack('>I', d[0x20:0x24])[0]
        shnum = struct.unpack('>H', d[0x30:0x32])[0]
        self.secs = []
        for i in range(shnum):
            s = struct.unpack('>10I', d[shoff+i*40:shoff+i*40+40])
            self.secs.append(s)
        shstr = self.secs[struct.unpack('>H', d[0x32:0x34])[0]]
        def nm(o):
            b = d[shstr[4]+o:]; return b[:b.index(b'\0')].decode('latin1')
        self.named = {nm(s[0]): s for s in self.secs}
        t = self.named['.text']; self.text_addr, self.text_off, self.text_size = t[3], t[4], t[5]
        self.maps = [(s[3], s[4], s[5]) for s in self.secs if s[3] and s[1] != 8 and s[4]]
        # symbols (imports)
        self.syms = {}
        st = self.named['.symtab']; ss = self.named['.strtab']
        for i in range(st[5]//16):
            n, v, sz, info, oth, sh = struct.unpack('>IIIBBH', d[st[4]+i*16:st[4]+i*16+16])
            b = d[ss[4]+n:]; name = b[:b.index(b'\0')].decode('latin1')
            if name and v: self.syms[v] = name
        # relocations for .text: map insn address -> (type, symaddr+addend)
        self.rel = {}
        symtab = []
        for i in range(st[5]//16):
            n, v, sz, info, oth, sh = struct.unpack('>IIIBBH', d[st[4]+i*16:st[4]+i*16+16])
            symtab.append(v)
        for rname in ('.rela.text', '.rela.data', '.rela.rodata'):
            r = self.named.get(rname)
            if not r: continue
            for i in range(r[5]//12):
                off, info, add = struct.unpack('>IIi', d[r[4]+i*12:r[4]+i*12+12])
                self.rel[off] = (info & 0xff, (symtab[info >> 8] + add) & 0xffffffff)

    def off(self, a):
        for base, o, sz in self.maps:
            if base <= a < base+sz: return o + a - base
        return None
    def u32(self, a): return struct.unpack('>I', self.d[self.off(a):self.off(a)+4])[0]
    def cstr(self, a):
        o = self.off(a)
        if o is None: return None
        e = self.d.index(b'\0', o); return self.d[o:e].decode('latin1')
    def words(self):
        t = self.d[self.text_off:self.text_off+self.text_size]
        return struct.unpack('>%dI' % (len(t)//4), t)

REG = lambda r: 'r%d' % r

def dis(w, pc):
    op = w >> 26
    rD = (w >> 21) & 31; rA = (w >> 16) & 31; rB = (w >> 11) & 31
    imm = w & 0xffff; si = s16(imm)
    if op == 18:
        li = w & 0x03fffffc
        if li & 0x02000000: li -= 0x04000000
        aa = (w >> 1) & 1; lk = w & 1
        tgt = li if aa else pc + li
        return ('bl' if lk else 'b'), tgt & 0xffffffff
    if op == 16:
        bd = w & 0xfffc
        if bd & 0x8000: bd -= 0x10000
        lk = w & 1
        bo, bi = rD, rA
        cond = {12: 'bt', 4: 'bf', 16: 'bdnz', 18: 'bdz', 20: 'b'}.get(bo & 0x1e, 'bc%d' % bo)
        crb = ['lt', 'gt', 'eq', 'so'][bi & 3]
        return '%s%s cr%d.%s' % (cond, 'l' if lk else '', bi >> 2, crb), (pc + bd) & 0xffffffff
    if op == 15: return ('lis %s,0x%x' % (REG(rD), imm)) if rA == 0 else ('addis %s,%s,0x%x' % (REG(rD), REG(rA), imm)), None
    if op == 14: return ('li %s,%d' % (REG(rD), si)) if rA == 0 else ('addi %s,%s,%d' % (REG(rD), REG(rA), si)), None
    if op == 24: return 'ori %s,%s,0x%x' % (REG(rA), REG(rD), imm), None
    if op == 11: return 'cmpwi cr%d,%s,%d' % (rD >> 2, REG(rA), si), None
    if op == 10: return 'cmplwi cr%d,%s,%d' % (rD >> 2, REG(rA), imm), None
    names = {32: 'lwz', 33: 'lwzu', 34: 'lbz', 35: 'lbzu', 36: 'stw', 37: 'stwu', 38: 'stb', 39: 'stbu',
             40: 'lhz', 41: 'lhzu', 42: 'lha', 44: 'sth', 45: 'sthu', 46: 'lmw', 47: 'stmw', 48: 'lfs', 50: 'lfd', 52: 'stfs', 54: 'stfd'}
    if op in names:
        return '%s %s,%d(%s)' % (names[op], ('f%d' % rD) if op >= 48 else REG(rD), si, REG(rA)), None
    if op == 31:
        xo = (w >> 1) & 0x3ff
        if xo == 444:
            return ('mr %s,%s' % (REG(rA), REG(rD))) if rD == rB else 'or %s,%s,%s' % (REG(rA), REG(rD), REG(rB)), None
        if xo == 339:
            spr = ((rB << 5) | rA); return 'mfspr %s,%d' % (REG(rD), spr) if spr not in (8, 9) else ('mflr %s' % REG(rD) if spr == 8 else 'mfctr %s' % REG(rD)), None
        if xo == 467:
            spr = ((rB << 5) | rA); return ('mtlr %s' % REG(rD)) if spr == 8 else ('mtctr %s' % REG(rD)) if spr == 9 else 'mtspr %d,%s' % (spr, REG(rD)), None
        if xo == 0: return 'cmpw cr%d,%s,%s' % (rD >> 2, REG(rA), REG(rB)), None
        if xo == 32: return 'cmplw cr%d,%s,%s' % (rD >> 2, REG(rA), REG(rB)), None
        if xo == 266: return 'add %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 40: return 'subf %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 23: return 'lwzx %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 151: return 'stwx %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 87: return 'lbzx %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 28: return 'and %s,%s,%s' % (REG(rA), REG(rD), REG(rB)), None
        if xo == 24: return 'slw %s,%s,%s' % (REG(rA), REG(rD), REG(rB)), None
        if xo == 26: return 'cntlzw %s,%s' % (REG(rA), REG(rD)), None
        if xo == 954: return 'extsb %s,%s' % (REG(rA), REG(rD)), None
        if xo == 922: return 'extsh %s,%s' % (REG(rA), REG(rD)), None
        if xo == 235: return 'mullw %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 491: return 'divw %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 459: return 'divwu %s,%s,%s' % (REG(rD), REG(rA), REG(rB)), None
        if xo == 104: return 'neg %s,%s' % (REG(rD), REG(rA)), None
        return 'op31_%d' % xo, None
    if op == 19:
        xo = (w >> 1) & 0x3ff
        if xo == 16: return ('blr' if not (w & 1) else 'blrl') if rD == 20 else 'bclr %d,%d' % (rD, rA), None
        if xo == 528: return 'bctr' if not (w & 1) else 'bctrl', None
        return 'op19_%d' % xo, None
    if op == 21:
        sh = rB; mb = (w >> 6) & 31; me = (w >> 1) & 31
        if mb == 0 and me == 31 - sh: return 'slwi %s,%s,%d' % (REG(rA), REG(rD), sh), None
        if sh == 0 and me == 31: return 'clrlwi %s,%s,%d' % (REG(rA), REG(rD), mb), None
        return 'rlwinm %s,%s,%d,%d,%d' % (REG(rA), REG(rD), sh, mb, me), None
    if op == 28: return 'andi. %s,%s,0x%x' % (REG(rA), REG(rD), imm), None
    if op == 7: return 'mulli %s,%s,%d' % (REG(rD), REG(rA), si), None
    if op == 8: return 'subfic %s,%s,%d' % (REG(rD), REG(rA), si), None
    if op == 12: return 'addic %s,%s,%d' % (REG(rD), REG(rA), si), None
    if op == 25: return 'oris %s,%s,0x%x' % (REG(rA), REG(rD), imm), None
    return '.word 0x%08x' % w, None
