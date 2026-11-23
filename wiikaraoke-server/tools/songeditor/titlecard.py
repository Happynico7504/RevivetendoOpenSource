"""
Generates the title-card PNG a custom song needs instead of borrowing a
template's — confirmed 2026-11-07 that the title/writer-credits/artist text
shown in-game is NOT read from any database or embedded string field at
all; it's baked directly into the song's own PNG background section as a
pre-rendered graphic (real example: a transparent 1280x720 image with
blue-filled, dark-outlined, light-highlighted text). Every custom song
needs its own version of this or it keeps showing whatever real song's
title card its template/font/records were borrowed from.
"""
from PIL import Image, ImageDraw, ImageFont

CANVAS_SIZE = (1280, 720)
FONT_PATH = "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"
FILL = (41, 41, 253, 255)
OUTLINE = (0, 0, 0, 160)
HIGHLIGHT = (220, 220, 255, 140)


def _draw_embossed(draw, xy, text, font):
    x, y = xy
    for dx, dy in ((-2, -2), (-2, 2), (2, -2), (2, 2), (-2, 0), (2, 0), (0, -2), (0, 2)):
        draw.text((x + dx, y + dy), text, font=font, fill=OUTLINE)
    draw.text((x - 1, y - 1), text, font=font, fill=HIGHLIGHT)
    draw.text(xy, text, font=font, fill=FILL)


def build(title, artist, writer=None, composer=None):
    """Returns PNG bytes for a title card matching the real games' layout:
    title top-center-ish, writer/composer credits below it, artist name
    lower-right with a note prefix."""
    img = Image.new("RGBA", CANVAS_SIZE, (0, 0, 0, 0))
    draw = ImageDraw.Draw(img)

    title_font = ImageFont.truetype(FONT_PATH, 72)
    credit_font = ImageFont.truetype(FONT_PATH, 32)
    artist_font = ImageFont.truetype(FONT_PATH, 48)

    title_w = draw.textlength(title, font=title_font)
    _draw_embossed(draw, (CANVAS_SIZE[0] / 2 - title_w / 2, 160), title, title_font)

    y = 280
    if writer:
        line = "Lyrics: %s" % writer
        _draw_embossed(draw, (370, y), line, credit_font)
        y += 48
    if composer:
        line = "Music: %s" % composer
        _draw_embossed(draw, (370, y), line, credit_font)

    artist_text = "♪  " + artist
    artist_w = draw.textlength(artist_text, font=artist_font)
    _draw_embossed(draw, (CANVAS_SIZE[0] - artist_w - 80, 540), artist_text, artist_font)

    import io
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    return buf.getvalue()
