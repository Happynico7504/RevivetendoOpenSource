#!/usr/bin/env python3
"""
Make the EU title launchable on a USA console by widening <region> in meta.xml.

  python3 tools/meta_region.py meta/meta.xml            -> USA+EUR (00000006)
  python3 tools/meta_region.py meta/meta.xml --all      -> all regions (FFFFFFFF)

Region bits: 1 = JPN, 2 = USA, 4 = EUR. Wii Karaoke U EU ships with 00000004.
The RPX itself has no region lock: the console's country (cafe.cntry_reg) is
only sent to the server as "cid" and used for the German rating notice and a
per-country song removal list. The original server rejected non-EU countries
(MSG_DIALOG_ERROR_NOT_SUPPORT_COUNTRY); this replacement server does not.

A modified meta.xml needs signature patches when installing (Aroma/Tiramisu
CFW). Alternative without touching files: a region-free plugin on Aroma.
"""
import re
import sys


def main(path, all_regions=False):
    xml = open(path, "r", encoding="utf-8").read()
    m = re.search(r'(<region[^>]*>)([0-9A-Fa-f]{8})(</region>)', xml)
    if not m:
        sys.exit("no <region> element found")
    old = int(m.group(2), 16)
    new = 0xFFFFFFFF if all_regions else (old | 0x2 | 0x4)
    xml = xml[:m.start(2)] + "%08X" % new + xml[m.end(2):]
    open(path, "w", encoding="utf-8").write(xml)
    print("region %08X -> %08X" % (old, new))


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1], "--all" in sys.argv)
