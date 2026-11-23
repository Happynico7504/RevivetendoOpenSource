#!/usr/bin/env python3
"""
Add one custom song to the REAL wiiudb.dat catalog, instead of
make_main_db.py's approach of replacing it with a minimal stub.

Confirmed by inspection: SONGS (0xb3783e9e) isn't the only table keyed by
song id. 11 other tables share its exact row count (2658 in the DE dump),
clearly parallel per-song data (search/browse sort-order variants of
[song_id, TITLE_UPPER, artist_id], plus two others: a timestamp/flags row
and a genre/difficulty-ish row). Emptying all of them (make_main_db.py's
approach) is what broke search and produced a playback-time error (ecode
1155919) once the song was actually selected - the real catalog's other
UI/search paths expect these tables to be populated, not empty.

This script keeps every table byte-for-byte except: it appends one row to
SONGS, ARTISTS, and each of the 11 song-keyed parallel tables. It reuses an
EXISTING real artist_id by default (avoiding the need to also touch the 9
artist-keyed parallel tables, which aren't needed for song search/playback)
unless --new-artist is passed.

  python3 tools/add_custom_song.py <dump>/content/global/tsv/de/wiiudb.dat out.bin \
      <song_id> "<title>" <artist_id> ["<artist name>"]
"""
import bisect
import re
import struct
import sys
import zlib

TABLE_HEADER_SIZE = 0x50
SONGS = 0xB3783E9E
ARTISTS = 0xA96E7E80


def _sort_key(s):
    """CONFIRMED (empirically, against the real title_rank/ARTISTS.col3
    order): the game's real alphabetical sort key strips all punctuation
    and whitespace before uppercasing - NOT a plain .upper(). A naive
    .upper() comparison is only ~86% monotonic against the real title_rank
    order; this sanitized key is ~96%, and correctly places a test title
    between its true alphabetical neighbors ("Crushed..." between "Cruella
    De Vil" and "Crush on You") where plain .upper() put it off by ~16
    positions, inside an unrelated "D" run."""
    return re.sub(r"[^A-Za-z0-9]", "", s).upper()

# [song_id, TITLE_UPPER, artist_id] search/browse sort-order variants.
TITLE_INDEX_TABLES = {
    0x2447936C, 0x27100A6D, 0x04940240, 0x9B9CE8D9,
    0x7A824535, 0xBF60595E, 0x3717A9CE, 0x5E5B1666, 0x2EF39EDD,
}
# [song_id, timestamp(u32), 0, 0, 3, 0]
TIMESTAMP_TABLE = 0x583D5F55
# [song_id, val1, val2, val3] - exact meaning unconfirmed, copy a plausible shape
MISC_TABLE = 0xC4E8EC95
# Flat lists of song ids for a "Featured"/"Highlighted" UI widget - single
# row, fixed column count, no row-count/string-heap math needed. Both
# already have one unused (0) slot in the real data, which we can just fill
# in place.
FEATURED_SONGS_TABLE = 0x4B0224EA
HIGHLIGHTED_SONGS_TABLE = 0x91379A15

# CONFIRMED ROOT CAUSE of the scrolling crash: [letter, count] - precomputed
# per-first-letter row counts the UI's alphabet scrollbar uses to work out
# how far to jump into the sorted song/artist lists. 37 rows each (0-9, A-Z,
# Ñ). Confirmed by summing: 0x88683FC2 sums to exactly SONGS' row count
# (songs bucketed by TITLE's first letter), 0x6C48405A also sums to SONGS'
# row count (songs bucketed by their ARTIST's first letter - the "browse by
# artist" view), 0xA9CEDE03 sums to ARTISTS' row count (artist list's own
# first-letter buckets). 0x10736E87 sums to 0 across every real row (an
# unused/reserved 4th index, left untouched here). These were never updated
# when a row got appended - the UI trusted a stale total and walked off the
# end of the now-undersized-looking bucket into whatever memory follows,
# which is consistent with a hard, undiagnosable console crash and not a
# title-level fault. Each is a single fixed-size row per letter (no
# string-heap growth needed) - just increment the matching letter's count.
SONGS_TITLE_LETTER_COUNT = 0x88683FC2
SONGS_ARTIST_LETTER_COUNT = 0x6C48405A
ARTISTS_LETTER_COUNT = 0xA9CEDE03

# Same shape, one drill-down level deeper: [TWO-letter prefix, count], 1406
# rows each (not every A-Z/A-Z combo occurs in the real data, only the ones
# that actually do). Also confirmed by summing: both sum to exactly SONGS'
# row count, mirroring SONGS_TITLE_LETTER_COUNT/SONGS_ARTIST_LETTER_COUNT
# one level down. Missed in the first pass - these are what actually caused
# the still-reproducing scrolling crash around any letter whose single-letter
# total now matches but whose own prefix sub-bucket (e.g. "NI") didn't get
# bumped, leaving THIS level inconsistent with the row count instead.
SONGS_TITLE_PREFIX_COUNT = 0x5F39CFFA
SONGS_ARTIST_PREFIX_COUNT = 0x8F4C63E9

# [1900 (constant flag, meaning unconfirmed), 1-indexed rank within this
# category, song_id] - 8 per-category/genre song-rank lists (row counts in
# the real data vary 222-1133, confirming each is a SUBSET, not a full
# song-table copy). CONFIRMED: emptying these to 0 rows (reasonable given
# every row dangled a nonexistent real song_id) fixed the catalog-open
# crash but caused a NEW main-menu hang - whatever builds the home
# screen's "collections" carousel reads from these and apparently can't
# handle an empty category. Since we don't know which category each of
# our own custom songs should fall into, every custom song gets appended
# to every one of these 8 lists - guarantees non-empty AND no dangling
# references, at the cost of not having real genre-based categorization
# for custom content (nothing we can fix without decompiling whatever
# assigns the real catalog's genre tags in the first place).
CATEGORY_SONG_LIST_TABLES = {
    0xA9F77040, 0x8A73786D, 0x157B92F4, 0xF4653F18,
    0x31872373, 0xB9F0D3E3, 0xD0BC6C4B, 0xA014E4F0,
}

# [artist_id, artist_name] - full artist-name browse/index tables, ALL 994
# real artists in every one of these (not a subset, unlike
# CATEGORY_SONG_LIST_TABLES above) - the artist-side equivalent of the same
# bug: emptied to 0 rows by make_empty_stub.py but never populated here,
# so a catalog-open/artist-browse/sort crash with a from-scratch catalog
# makes sense even with ARTISTS itself correctly populated, since this is
# a SEPARATE, parallel index the UI apparently also needs. 0x20B8B391 uses
# mixed-case display names in the real data ("Lady Gaga") while every
# other one here uses uppercase ("LADY GAGA") - preserve that distinction
# rather than uppercasing everything uniformly.
ARTIST_LIST_TABLES = {
    0x35057E59, 0x16817674, 0x89899CED, 0x68973101,
    0xAD752D6A, 0x2502DDFA, 0x4C4E6252, 0x3CE6EAE9,
    0x3E51D372,
}
ARTIST_LIST_TABLE_MIXED_CASE = 0x20B8B391


def _table(d, start, size):
    return bytearray(d[start:start + size])


def _bump_letter_count(table, letter):
    """Increment the row whose string column matches `letter` by 1. Fixed
    row size/count, no string-heap growth - a plain in-place rewrite."""
    rowsize = table[0x2C]
    rows = struct.unpack_from(">I", table, 0x40)[0]
    for i in range(rows):
        row_off = TABLE_HEADER_SIZE + i * rowsize
        str_off = struct.unpack_from(">I", table, row_off)[0]
        p = TABLE_HEADER_SIZE + str_off
        e = table.index(b"\0", p)
        if table[p:e].decode("utf-8", "replace") == letter:
            cnt_off = row_off + 4
            cnt = struct.unpack_from(">I", table, cnt_off)[0]
            struct.pack_into(">I", table, cnt_off, cnt + 1)
            return True
    return False


def _bump_or_add_bucket_count(table, key, types):
    """Like _bump_letter_count, but for tables that don't cover every
    possible key in the real data (the two-letter prefix tables only have
    rows for prefixes that actually occur) - append a new [key, 1] row via
    the normal string-heap-growth path when no existing row matches."""
    if _bump_letter_count(table, key):
        return table
    return _append_row(table, [key, 1], types, None)


def _find_artist_name(d, artist_id):
    """Read-only walk of the directory to fetch an EXISTING artist's name
    (needed for SONGS_ARTIST_LETTER_COUNT when reusing an artist_id rather
    than creating a new one, where artist_name is never passed in)."""
    _, _, count = struct.unpack_from(">III", d, 0)
    pos = 12 + count * 12
    for i in range(count):
        size, name, _ = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        if name == ARTISTS:
            table = d[start:start + size]
            rowsize = table[0x2C]
            types = list(table[0x2D:0x2D + rowsize // 4])
            rows = struct.unpack_from(">I", table, 0x40)[0]
            for r in range(rows):
                vals = struct.unpack_from(">%dI" % (rowsize // 4), table, TABLE_HEADER_SIZE + r * rowsize)
                if vals[0] == artist_id:
                    off = TABLE_HEADER_SIZE + vals[1]
                    e = table.index(b"\0", off)
                    return table[off:e].decode("utf-8", "replace")
            return None
        pos = start + size
    return None


def _rank_insert_pos(table, rowsize, rank_col, key_col, new_key):
    """Where our new row's rank value belongs. CONFIRMED by testing two
    other approaches against the real catalog first: a plain count of
    existing keys < new_key undercounts whenever a smaller-key row already
    sits exactly at that count's boundary due to the real data's own ~4-8%
    disorder (reproduced: it placed a test artist ("Nico Christmann") one
    slot BEFORE "Michael Jackson", who sorts earlier) - and the opposite
    fix, "1 + max rank among smaller keys", is wrecked by a single far-away
    outlier (reproduced: one mis-ranked real title pulled a C-titled test
    song all the way into the W's). Sorting existing rows by their ACTUAL
    rank value and bisecting that (already almost-sorted) key sequence is
    stable against both failure modes and reproduced the correct local
    neighbors in every test case."""
    rows = struct.unpack_from(">I", table, 0x40)[0]
    pairs = []
    for i in range(rows):
        base = TABLE_HEADER_SIZE + i * rowsize
        rank_val = struct.unpack_from(">I", table, base + rank_col * 4)[0]
        str_off = struct.unpack_from(">I", table, base + key_col * 4)[0]
        p = TABLE_HEADER_SIZE + str_off
        e = table.index(b"\0", p)
        pairs.append((rank_val, _sort_key(table[p:e].decode("utf-8", "replace"))))
    pairs.sort(key=lambda x: x[0])
    keys_by_rank = [k for _, k in pairs]
    return bisect.bisect_left(keys_by_rank, new_key)


def _insert_rank(table, rowsize, rank_col, insert_pos):
    """CONFIRMED ROOT CAUSE of the still-reproducing scrolling crash (letter
    buttons work, continuous scroll doesn't): title_rank/artist_rank (and
    MISC_TABLE's equivalent pair) aren't just unique ids - sorting the real
    catalog by either one reproduces actual alphabetical order almost
    exactly (first-by-rank starts with digits, last-by-rank starts with Z).
    They're literal 0-indexed alphabetical positions that smooth scrolling
    resolves directly by rank, unlike letter-jump which only needs correct
    bucket totals. Appending "next free index" placed our row after EVERY
    real entry alphabetically instead of at its true position, leaving
    every song after it off-by-one between its bucket's cumulative offset
    and its own unrenumbered rank. Fix: make room at the real insertion
    point by shifting every existing rank >= insert_pos up by one, exactly
    like inserting into a sorted array."""
    rows = struct.unpack_from(">I", table, 0x40)[0]
    for i in range(rows):
        off = TABLE_HEADER_SIZE + i * rowsize + rank_col * 4
        v = struct.unpack_from(">I", table, off)[0]
        if v >= insert_pos:
            struct.pack_into(">I", table, off, v + 1)


def _append_row(table, row_values, types, str_table_rows_known_header):
    # Real layout (confirmed by inspection, NOT interleaved per-row): a
    # fixed-size row array of old_rows*rowsize bytes, followed by a single
    # string heap for every row's string columns. String offsets stored in
    # the fixed rows are absolute, relative to 0x50. Inserting a new row
    # therefore has to: grow the fixed array by one rowsize (pushing the
    # whole string heap later by rowsize bytes, so every EXISTING row's
    # stored string offset needs +rowsize), then append the new row's own
    # string(s) at the very end of the (now relocated) heap.
    header = table[:TABLE_HEADER_SIZE]
    rowsize = header[0x2C]
    old_rows, old_data_size = struct.unpack_from(">II", header, 0x40)
    body = bytearray(table[TABLE_HEADER_SIZE:TABLE_HEADER_SIZE + old_data_size])
    fixed_size = old_rows * rowsize
    fixed_part = body[:fixed_size]
    string_heap = body[fixed_size:]

    for i in range(old_rows):
        for ci, t in enumerate(types):
            if t == 1:
                off = i * rowsize + ci * 4
                v = struct.unpack_from(">I", fixed_part, off)[0]
                # 0 is a real sentinel for "empty string", not a heap offset
                # (confirmed: every existing row's empty-string column is
                # stored as exactly 0) - relocating it to `rowsize` turns it
                # into a bogus non-zero offset pointing at garbage, which
                # silently corrupts every pre-existing row the moment any
                # row gets appended to this table.
                if v != 0:
                    struct.pack_into(">I", fixed_part, off, v + rowsize)

    new_fixed_size = fixed_size + rowsize
    row = bytearray(rowsize)
    strs = []
    for ci, (t, v) in enumerate(zip(types, row_values)):
        if t == 1:
            # Match the real format's own convention: an empty string is the
            # 0 sentinel, not a real offset to an empty C-string.
            if v == "":
                struct.pack_into(">I", row, ci * 4, 0)
            else:
                strs.append((ci, v))
        else:
            struct.pack_into(">I", row, ci * 4, v)
    str_bytes = bytearray()
    for ci, s in strs:
        off = new_fixed_size + len(string_heap) + len(str_bytes)
        struct.pack_into(">I", row, ci * 4, off)
        str_bytes += s.encode("utf-8") + b"\0"

    new_body = bytes(fixed_part) + bytes(row) + bytes(string_heap) + bytes(str_bytes)
    new_body += b"\0" * (-len(new_body) % 16)
    new_table = bytearray(header) + new_body
    struct.pack_into(">II", new_table, 0x40, old_rows + 1, len(new_body))
    return bytes(new_table)


def _insert_row_by_id(table, row_values, types):
    """Same string-heap-growth logic as _append_row, but physically
    inserts the new row at the position that keeps the table sorted
    ascending by its own first column (an id), instead of always
    appending at the end.

    CONFIRMED (checked the real, unmodified disc dump): both SONGS and
    ARTISTS are physically stored in strict ascending-id row order, not
    just title/name-sorted via the rank columns. A catalog-open crash
    traced via WiiUCrashLogDumper kept returning null from a lookup
    keyed by our custom artist_id, even after every other
    rank/bucket/category fix landed and with NEX disabled to rule out
    anything network-related - consistent with a binary search over
    SONGS/ARTISTS by id, which only works if the table is physically
    id-sorted. _append_row always added the new row at the very end,
    with a hash-derived artist_id that has no relationship to title
    insertion order, so the table was never actually sorted by id at
    all. Inserting at the correct id-sorted position doesn't need any
    extra string-offset handling beyond what _append_row already does -
    growing the fixed-row region by one rowsize always pushes the
    string heap later by the same amount regardless of WHERE in the
    fixed-row array the new row physically lands."""
    header = table[:TABLE_HEADER_SIZE]
    rowsize = header[0x2C]
    old_rows, old_data_size = struct.unpack_from(">II", header, 0x40)
    body = bytearray(table[TABLE_HEADER_SIZE:TABLE_HEADER_SIZE + old_data_size])
    fixed_size = old_rows * rowsize
    fixed_part = body[:fixed_size]
    string_heap = body[fixed_size:]

    new_id = row_values[0]
    insert_at = old_rows
    for i in range(old_rows):
        existing_id = struct.unpack_from(">I", fixed_part, i * rowsize)[0]
        if existing_id > new_id:
            insert_at = i
            break

    for i in range(old_rows):
        for ci, t in enumerate(types):
            if t == 1:
                off = i * rowsize + ci * 4
                v = struct.unpack_from(">I", fixed_part, off)[0]
                if v != 0:
                    struct.pack_into(">I", fixed_part, off, v + rowsize)

    new_fixed_size = fixed_size + rowsize
    row = bytearray(rowsize)
    strs = []
    for ci, (t, v) in enumerate(zip(types, row_values)):
        if t == 1:
            if v == "":
                struct.pack_into(">I", row, ci * 4, 0)
            else:
                strs.append((ci, v))
        else:
            struct.pack_into(">I", row, ci * 4, v)
    str_bytes = bytearray()
    for ci, s in strs:
        off = new_fixed_size + len(string_heap) + len(str_bytes)
        struct.pack_into(">I", row, ci * 4, off)
        str_bytes += s.encode("utf-8") + b"\0"

    insert_byte = insert_at * rowsize
    new_fixed_part = bytes(fixed_part[:insert_byte]) + bytes(row) + bytes(fixed_part[insert_byte:])
    new_body = new_fixed_part + bytes(string_heap) + bytes(str_bytes)
    new_body += b"\0" * (-len(new_body) % 16)
    new_table = bytearray(header) + new_body
    struct.pack_into(">II", new_table, 0x40, old_rows + 1, len(new_body))
    return bytes(new_table)


def build(path, out_path, song_id, title, artist_id, artist_name=None):
    d = open(path, "rb").read()
    version, checksum, count = struct.unpack_from(">III", d, 0)
    # RETRACTED: this used to overwrite version with int(time.time()) to make
    # every rebuild "look new" to some client-side cache. CONFIRMED WRONG: an
    # independent community parser (and direct verification against the real
    # disc file) shows the client requires this field to be the exact
    # constant 3 - any other value (including a timestamp) gets the whole
    # file rejected outright. The GD0001 freshness signal is already handled
    # content-wise by the server's own MD5-based "id" (wahp/content.py), so
    # there was never a need to fake this field - leave version AND checksum
    # exactly as read from the source file, matching the disc's own
    # convention (checksum is separately confirmed unvalidated by the
    # client, but there's no reason to change it either).
    song_title_letter = title[:1].upper()
    existing_artist_name = artist_name if artist_name is not None else _find_artist_name(d, artist_id)
    song_artist_letter = (existing_artist_name or "")[:1].upper()
    song_title_prefix = title[:2].upper()
    song_artist_prefix = (existing_artist_name or "")[:2].upper()

    # Read-only pass over the ORIGINAL SONGS table to find where our song's
    # title_rank actually belongs (see _insert_rank/_sort_key's docstrings).
    # Reused for both SONGS itself and MISC_TABLE below - both carry the
    # same title-order permutation. NOTE: SONGS.artist_rank (col 14) and
    # MISC_TABLE's equivalent column deliberately do NOT get this treatment
    # - empirically they're only ~58% monotonic under every artist-name key
    # tried (unlike title_rank's ~96% and ARTISTS.col3's ~92%), so their
    # real sort criterion is still unknown. Inserting at a confidently-wrong
    # position would be worse than the old "next free unique index" append,
    # which at least doesn't collide with anything - so those two keep the
    # old behavior below.
    title_insert_pos = None
    _, _, _count0 = struct.unpack_from(">III", d, 0)
    _pos0 = 12 + _count0 * 12
    for _i in range(_count0):
        _size0, _name0, _ = struct.unpack_from(">III", d, 12 + _i * 12)
        _start0 = (_pos0 + 15) & ~15
        if _name0 == SONGS:
            _songs_tbl = d[_start0:_start0 + _size0]
            _rowsize0 = _songs_tbl[0x2C]
            title_insert_pos = _rank_insert_pos(_songs_tbl, _rowsize0, 13, 1, _sort_key(title))
            break
        _pos0 = _start0 + _size0

    entries, tables = [], []
    pos = 12 + count * 12
    for i in range(count):
        size, name, one = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        table = _table(d, start, size)
        rowsize = table[0x2C]
        types = list(table[0x2D:0x2D + rowsize // 4])

        if name == SONGS:
            # Real row shape (confirmed from the DE wiiudb.dat): [id, title,
            # artist_id, 2, year, 1, 1, 1, id(again), "", sort-letter, "0",
            # 0, title_rank, artist_rank]. title_rank (col 13) is a REAL
            # 0-indexed alphabetical-position rank (see _insert_rank/
            # _sort_key) - insert at the true position, shifting every
            # existing song's title_rank up by one. artist_rank (col 14)
            # keeps the old "next free unique index" append (see the
            # title_insert_pos comment above for why).
            _insert_rank(table, rowsize, 13, title_insert_pos)
            next_artist_rank = struct.unpack_from(">I", table, 0x40)[0]
            values = [song_id, title, artist_id, 2, 2026, 1, 1, 1,
                      song_id, "", title[:1].upper(), "0", 0, title_insert_pos, next_artist_rank]
            table = _insert_row_by_id(table, values, types)
        elif name == ARTISTS and artist_name is not None:
            # Column 3 is the same kind of real alphabetical-position rank
            # (confirmed: sorting ARTISTS by col3 with _sort_key reproduces
            # alphabetical artist-name order ~92% monotonically). Column 4
            # does NOT (no alphabetical pattern under any key tried) - some
            # other/unused value, so it keeps the old "next free unique
            # index" behavior.
            next_rank = struct.unpack_from(">I", table, 0x40)[0]
            artist_rank_pos = _rank_insert_pos(table, rowsize, 3, 1, _sort_key(artist_name))
            _insert_rank(table, rowsize, 3, artist_rank_pos)
            values = [artist_id, artist_name, 8, artist_rank_pos, next_rank]
            table = _insert_row_by_id(table, values, types)
        elif name in ARTIST_LIST_TABLES and artist_name is not None:
            values = [artist_id, artist_name.upper()]
            table = _append_row(table, values, types, None)
        elif name == ARTIST_LIST_TABLE_MIXED_CASE and artist_name is not None:
            values = [artist_id, artist_name]
            table = _append_row(table, values, types, None)
        elif name in TITLE_INDEX_TABLES:
            values = [song_id, title.upper(), artist_id]
            table = _append_row(table, values, types, None)
        elif name == TIMESTAMP_TABLE:
            values = [song_id, 1700000000, 0, 0, 3, 0]
            table = _insert_row_by_id(table, values, types)
        elif name == MISC_TABLE:
            # Confirmed by inspection: column 1 carries the exact same
            # title-order permutation as SONGS.title_rank (sorting
            # MISC_TABLE by it reproduces the same alphabetical order) -
            # same insert, not append. Column 2 parallels SONGS.artist_rank
            # (unconfirmed real criterion) - same "next free index" append.
            _insert_rank(table, rowsize, 1, title_insert_pos)
            next_index = struct.unpack_from(">I", table, 0x40)[0]
            values = [song_id, title_insert_pos, next_index, 2]
            table = _insert_row_by_id(table, values, types)
        elif name in (FEATURED_SONGS_TABLE, HIGHLIGHTED_SONGS_TABLE):
            # Single row, fixed columns, all plain ints (types all 3) - find
            # the first 0 slot and overwrite it with our song_id in place.
            # Row count/data size/string heap are untouched.
            #
            # CONFIRMED (checked the real, unmodified stock data): both
            # tables always carry EXACTLY ONE zero slot, and it isn't even
            # positioned last (HIGHLIGHTED's zero sat in slot 0, not slot
            # 5) - so this isn't a "scan for a terminator" structure, the
            # invariant is just "exactly one empty slot, anywhere, always".
            # A single add_custom_song.py call can't maintain that on its
            # own (it doesn't know the eventual total song count), so this
            # does the simple, locally-sensible thing - fill the first
            # zero slot, same as the original behavior - and
            # build_catalog_from_tsv.py's fill_featured_slots() does a
            # proper final pass afterwards that overwrites this table
            # entirely, cycling through the real song list so it ends up
            # with every slot filled except exactly one.
            row_count = struct.unpack_from(">I", table, 0x40)[0]
            for ci in range(rowsize // 4):
                off = TABLE_HEADER_SIZE + ci * 4
                if struct.unpack_from(">I", table, off)[0] == 0:
                    struct.pack_into(">I", table, off, song_id)
                    break
            assert row_count == 1, "unexpected row count for a featured/highlighted table"
        elif name == SONGS_TITLE_LETTER_COUNT:
            if not _bump_letter_count(table, song_title_letter):
                print("warning: no letter bucket for title %r (%r) in "
                      "SONGS_TITLE_LETTER_COUNT - scroll index now undercounts"
                      % (song_title_letter, title), file=sys.stderr)
        elif name == SONGS_ARTIST_LETTER_COUNT:
            if not _bump_letter_count(table, song_artist_letter):
                print("warning: no letter bucket for artist %r (%r) in "
                      "SONGS_ARTIST_LETTER_COUNT - scroll index now undercounts"
                      % (song_artist_letter, existing_artist_name), file=sys.stderr)
        elif name == ARTISTS_LETTER_COUNT and artist_name is not None:
            artist_letter = artist_name[:1].upper()
            if not _bump_letter_count(table, artist_letter):
                print("warning: no letter bucket for artist %r (%r) in "
                      "ARTISTS_LETTER_COUNT - scroll index now undercounts"
                      % (artist_letter, artist_name), file=sys.stderr)
        elif name == SONGS_TITLE_PREFIX_COUNT:
            table = _bump_or_add_bucket_count(table, song_title_prefix, types)
        elif name == SONGS_ARTIST_PREFIX_COUNT:
            table = _bump_or_add_bucket_count(table, song_artist_prefix, types)
        elif name in CATEGORY_SONG_LIST_TABLES:
            next_rank = struct.unpack_from(">I", table, 0x40)[0] + 1  # 1-indexed in the real data
            values = [1900, next_rank, song_id]
            table = _append_row(table, values, types, None)
        # every other table: pass through unmodified, byte-for-byte

        entries.append(struct.pack(">III", len(table), name, one))
        tables.append(bytes(table))
        pos = start + size

    out = bytearray(struct.pack(">III", version, checksum, count) + b"".join(entries))
    for table in tables:
        out += b"\0" * (-len(out) % 16) + table
    # CONFIRMED BUG (found via an independent community parser rejecting our
    # output as short): the directory's own "size" field for the LAST table
    # excludes a trailing 32-byte block that holds that table's build-date
    # stamp - every OTHER table's trailing 32 bytes get read as part of the
    # NEXT table's leading slice (since the next _table() call starts from
    # `pos = start + size`, which already covers them), but the very last
    # table has no "next" slice to carry its own trailer into, so it was
    # silently dropped on every single build. Preserve it exactly by copying
    # the real tail of the source file.
    out += d[pos:]
    # CONFIRMED (decompiled FUN_0210e2a0, the real client-side validator, and
    # verified against the real disc file): the LAST 32 bytes of the whole
    # file are a trailer whose first 4 bytes hold a CRC32 of everything
    # before those 32 bytes, stored byte-swapped (so a big-endian *(u32*)
    # read needs a byteswap to match a standard CRC32). Preserving the
    # original trailer bytes verbatim (the fix above) keeps the date-stamp
    # string in bytes 4-32, but the CRC in bytes 0-4 is now STALE - it was
    # computed for the pre-edit file, and every table edit above changes
    # bytes before this trailer, so it no longer matches. Recompute it.
    crc = zlib.crc32(bytes(out[:-32])) & 0xFFFFFFFF
    swapped = struct.unpack("<I", struct.pack(">I", crc))[0]
    struct.pack_into(">I", out, len(out) - 32, swapped)
    open(out_path, "wb").write(bytes(out))
    print("wrote", out_path, len(out), "bytes")


if __name__ == "__main__":
    if len(sys.argv) not in (6, 7):
        sys.exit(__doc__)
    build(sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], int(sys.argv[5]),
          sys.argv[6] if len(sys.argv) == 7 else None)
