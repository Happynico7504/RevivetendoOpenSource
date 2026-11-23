"""
Custom song editor/builder for Wii Karaoke U.

Scaffolding only so far — covers everything confirmed on real hardware this
session: the UJK0 container, SSZL compression, the JOY-U1 track event format
(including the melody/vocal guide-note opcodes), and the streamed TPSA audio
packet format. Lyric-sweep timing and the scrolling pitch-corridor are NOT
implemented yet (see track.py) — songs built with this package currently
show lyrics/corridor verbatim from whatever template song they borrow those
sections from, not from the editor's own input.
"""
