import struct, zlib, sys
src, dst = sys.argv[1], sys.argv[2]
d = open(src,'rb').read()
shoff = struct.unpack('>I', d[0x20:0x24])[0]
shnum = struct.unpack('>H', d[0x30:0x32])[0]
shstrndx = struct.unpack('>H', d[0x32:0x34])[0]
secs=[]
for i in range(shnum):
    s = list(struct.unpack('>10I', d[shoff+i*40:shoff+i*40+40]))
    secs.append(s)
out = bytearray(d[:0x34]); 
datas=[]
for s in secs:
    name,typ,flags,addr,off,size,link,info,align,es = s
    if typ==8 or size==0: datas.append(b''); continue
    raw = d[off:off+size]
    if flags & 0x08000000:
        raw = zlib.decompress(raw[4:]); s[2] = flags & ~0x08000000
    datas.append(raw)
# layout: header, section headers, data
pos = 0x40 + shnum*40
pos = (pos+63)&~63
body=bytearray()
for s,raw in zip(secs,datas):
    if raw:
        s[4]=pos+len(body); s[5]=len(raw); body+=raw
        while len(body)%64: body+=b'\0'
    elif s[1]!=8: s[4]=0
o = bytearray(d[:0x34]); o += b'\0'*(0x40-len(o))
struct.pack_into('>I',o,0x20,0x40)
for s in secs: o+=struct.pack('>10I',*s)
while len(o)<pos: o+=b'\0'
o+=body
open(dst,'wb').write(o)
print("ok", len(o))
