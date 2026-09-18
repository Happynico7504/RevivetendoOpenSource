#!/usr/bin/env python3
"""Revivetendo Bot — community Discord bot for the Revivetendo Pretendo Bridge."""

import array
import asyncio
import base64
import io
import os
import random
import secrets
import tempfile
import threading
import time as _time
import wave
from datetime import datetime, time, timedelta, timezone

import discord
from discord import app_commands
from discord.ext import commands, tasks, voice_recv
import davey
from piper import PiperVoice
import psycopg2
import aiohttp
from aiohttp import web, ClientSession

# ──────────────────────────────────────────────
# Config — loaded from .env in the same directory
# ──────────────────────────────────────────────

_env_path = os.path.join(os.path.dirname(__file__), ".env")
if os.path.exists(_env_path):
    with open(_env_path) as _f:
        for _line in _f:
            _line = _line.strip()
            if _line and not _line.startswith("#") and "=" in _line:
                _k, _v = _line.split("=", 1)
                os.environ.setdefault(_k.strip(), _v.strip())

TOKEN           = os.environ["DISCORD_BOT_TOKEN"]
GUILD_ID        = int(os.environ["DISCORD_GUILD_ID"])
MEMBER_ROLE_ID  = int(os.environ["DISCORD_MEMBER_ROLE_ID"])
MODERATOR_ROLE_ID = int(os.environ["DISCORD_MODERATOR_ROLE_ID"])
MOD_LOG_ID          = int(os.environ.get("DISCORD_MOD_LOG_CHANNEL_ID", "0"))
ACTIVITY_LOG_ID     = int(os.environ.get("DISCORD_ACTIVITY_LOG_CHANNEL_ID", "0"))
RING_FALLBACK_CH_ID = int(os.environ.get("DISCORD_RING_FALLBACK_CHANNEL_ID", "0"))
MII_OF_DAY_CHANNEL_ID = int(os.environ.get("DISCORD_MII_OF_THE_DAY_CHANNEL_ID", "0"))
DB_URL          = os.environ.get("DATABASE_URL", "postgres://postgres:wiiu@localhost:5432/wiiuchat?sslmode=disable")
RING_PORT       = int(os.environ.get("RING_HTTP_PORT", "9203"))

# ──────────────────────────────────────────────
# Database
# ──────────────────────────────────────────────

def db_conn():
    return psycopg2.connect(DB_URL)

def db_init():
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute("""
                ALTER TABLE wii_devices ADD COLUMN IF NOT EXISTS discord_id TEXT;

                CREATE TABLE IF NOT EXISTS discord_link_codes (
                    code       TEXT PRIMARY KEY,
                    pnid       TEXT NOT NULL,
                    expires_at TIMESTAMP WITH TIME ZONE NOT NULL
                );

                CREATE TABLE IF NOT EXISTS discord_warns (
                    id           SERIAL PRIMARY KEY,
                    discord_id   TEXT NOT NULL,
                    moderator_id TEXT NOT NULL,
                    reason       TEXT NOT NULL,
                    created_at   TIMESTAMP WITH TIME ZONE DEFAULT now()
                );
            """)
        conn.commit()
    print("[bot] database schema ready", flush=True)

# ──────────────────────────────────────────────
# Bot setup
# ──────────────────────────────────────────────

intents = discord.Intents.default()
intents.members = True
intents.message_content = True

bot = commands.Bot(command_prefix="!", intents=intents)
_guild = discord.Object(id=GUILD_ID)

# Local TTS (Piper, fully offline - no cloud API). Loaded once at startup since
# loading the model takes ~1.5s; synthesis itself is ~0.3s/sentence once warm.
piper_voice = PiperVoice.load(os.path.join(os.path.dirname(__file__), "piper-voices", "en_US-lessac-medium.onnx"))

def _now():
    return datetime.now(timezone.utc)

async def _mod_log(embed: discord.Embed):
    if MOD_LOG_ID and (ch := bot.get_channel(MOD_LOG_ID)):
        await ch.send(embed=embed)

async def _activity_log(embed: discord.Embed):
    if ACTIVITY_LOG_ID and (ch := bot.get_channel(ACTIVITY_LOG_ID)):
        await ch.send(embed=embed)

def is_moderator():
    async def predicate(interaction: discord.Interaction) -> bool:
        member = interaction.user
        return isinstance(member, discord.Member) and any(r.id == MODERATOR_ROLE_ID for r in member.roles)
    return app_commands.check(predicate)

@bot.tree.error
async def on_app_command_error(interaction: discord.Interaction, error: app_commands.AppCommandError):
    if isinstance(error, app_commands.CheckFailure):
        await interaction.response.send_message("🚫 You need the Moderator role to use this command.", ephemeral=True)
        return
    raise error

# ──────────────────────────────────────────────
# Events
# ──────────────────────────────────────────────

@bot.event
async def on_ready():
    print(f"[bot] logged in as {bot.user} (id={bot.user.id})", flush=True)
    try:
        synced = await bot.tree.sync(guild=_guild)
        print(f"[bot] slash commands synced: {[c.name for c in synced]}", flush=True)
    except Exception as e:
        print(f"[bot] slash command sync FAILED: {e}", flush=True)
    if not mii_of_the_day.is_running():
        mii_of_the_day.start()

@bot.event
async def on_member_join(member: discord.Member):
    if member.guild.id != GUILD_ID:
        return
    role = member.guild.get_role(MEMBER_ROLE_ID)
    if role:
        try:
            await member.add_roles(role, reason="Auto-assigned on join")
        except discord.Forbidden:
            print(f"[bot] missing permission to assign member role to {member}", flush=True)
    embed = discord.Embed(
        title="Member Joined",
        description=f"{member.mention} **{member}**",
        color=0x57F287,
        timestamp=_now(),
    )
    embed.set_thumbnail(url=member.display_avatar.url)
    embed.add_field(name="Account created", value=discord.utils.format_dt(member.created_at, "R"))
    embed.set_footer(text=f"ID: {member.id}")
    await _activity_log(embed)

@bot.event
async def on_member_remove(member: discord.Member):
    if member.guild.id != GUILD_ID:
        return
    embed = discord.Embed(
        title="Member Left",
        description=f"**{member}**",
        color=0xFEE75C,
        timestamp=_now(),
    )
    embed.set_footer(text=f"ID: {member.id}")
    await _activity_log(embed)

@bot.event
async def on_message_delete(message: discord.Message):
    if not message.guild or message.guild.id != GUILD_ID or message.author.bot:
        return
    embed = discord.Embed(
        title="Message Deleted",
        description=f"**Author:** {message.author.mention} · **Channel:** {message.channel.mention}",
        color=0xED4245,
        timestamp=_now(),
    )
    if message.content:
        embed.add_field(name="Content", value=message.content[:1024], inline=False)
    embed.set_footer(text=f"Author ID: {message.author.id}")
    await _activity_log(embed)

@bot.event
async def on_message_edit(before: discord.Message, after: discord.Message):
    if not before.guild or before.guild.id != GUILD_ID or before.author.bot:
        return
    if before.content == after.content:
        return
    embed = discord.Embed(
        title="Message Edited",
        description=f"**Author:** {before.author.mention} · **Channel:** {before.channel.mention} · [Jump]({after.jump_url})",
        color=0xFEE75C,
        timestamp=_now(),
    )
    embed.add_field(name="Before", value=before.content[:512] or "*(empty)*", inline=False)
    embed.add_field(name="After",  value=after.content[:512]  or "*(empty)*", inline=False)
    embed.set_footer(text=f"Author ID: {before.author.id}")
    await _activity_log(embed)

# ──────────────────────────────────────────────
# Slash commands — PNID linking
# ──────────────────────────────────────────────

@bot.tree.command(guild=_guild, name="link_pnid", description="Link your Revivetendo PNID to Discord for WiiU Chat call notifications")
@app_commands.describe(code="The 16-character hex code shown on the Revivetendo website")
async def link_pnid(interaction: discord.Interaction, code: str):
    code = code.strip().upper()
    if len(code) != 16 or not all(c in "0123456789ABCDEF" for c in code):
        await interaction.response.send_message("❌ The code must be exactly 16 hex characters.", ephemeral=True)
        return
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT pnid FROM discord_link_codes WHERE code = %s AND expires_at > now()",
                (code,),
            )
            row = cur.fetchone()
            if not row:
                await interaction.response.send_message(
                    "❌ Invalid or expired code. Visit the Revivetendo website to generate a new one.",
                    ephemeral=True,
                )
                return
            pnid = row[0]
            cur.execute("UPDATE wii_devices SET discord_id = %s WHERE username = %s", (str(interaction.user.id), pnid))
            cur.execute("DELETE FROM discord_link_codes WHERE code = %s", (code,))
        conn.commit()
    await interaction.response.send_message(
        f"✅ Your Discord is now linked to PNID **{pnid}**.\nYou'll receive WiiU Chat call notifications here when someone calls you.",
        ephemeral=True,
    )
    print(f"[bot] linked discord_id={interaction.user.id} ({interaction.user}) → pnid={pnid}", flush=True)


_ACCOUNT_PROXY_SET_PASSWORD_URL = "http://127.0.0.1:9191/internal/web/set-password"

@bot.tree.command(guild=_guild, name="reset_web_password", description="Reset your Juxt/relay-admin web password (only works if one is already set)")
async def reset_web_password(interaction: discord.Interaction):
    await interaction.response.defer(ephemeral=True)

    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT username, web_password_hash FROM wii_devices WHERE discord_id = %s", (str(interaction.user.id),))
            row = cur.fetchone()
    if not row:
        await interaction.followup.send("❌ No PNID linked. Use `/link_pnid` first.", ephemeral=True)
        return
    pnid, web_password_hash = row
    if not web_password_hash:
        await interaction.followup.send(
            "❌ You don't have a web password set yet — this command only resets an existing one. Ask an admin to set one for you first.",
            ephemeral=True,
        )
        return

    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT pid FROM pnid_cache WHERE pnid = %s", (pnid,))
            pid_row = cur.fetchone()
    if not pid_row:
        await interaction.followup.send("❌ Couldn't resolve your PID — try again later or contact an admin.", ephemeral=True)
        return
    pid = pid_row[0]

    new_password = secrets.token_urlsafe(12)
    try:
        async with ClientSession() as session:
            async with session.post(
                _ACCOUNT_PROXY_SET_PASSWORD_URL,
                data={"pid": str(pid), "password": new_password},
                timeout=aiohttp.ClientTimeout(total=15),
            ) as resp:
                if resp.status != 200:
                    text = await resp.text()
                    print(f"[bot] reset_web_password: HTTP {resp.status} for PID {pid}: {text}", flush=True)
                    await interaction.followup.send("❌ Failed to reset password, try again later.", ephemeral=True)
                    return
    except Exception as e:
        print(f"[bot] reset_web_password error for PID {pid}: {e}", flush=True)
        await interaction.followup.send("❌ Failed to reset password, try again later.", ephemeral=True)
        return

    await interaction.followup.send(
        f"✅ Your web password for **{pnid}** has been reset.\nNew password: `{new_password}`\n"
        "Save it now — it won't be shown again.",
        ephemeral=True,
    )
    print(f"[bot] reset_web_password: pnid={pnid} via discord_id={interaction.user.id}", flush=True)


_ACCOUNT_PROXY_MII_URL = "http://127.0.0.1:9191/internal/mii"

async def _fetch_pretendo_mii(pnid: str, session: ClientSession):
    """Fetch FFLStoreData via account-proxy (authenticated). Returns (mii_bytes, mii_name) or (None, None)."""
    try:
        async with session.get(
            _ACCOUNT_PROXY_MII_URL,
            params={"pnid": pnid},
            timeout=aiohttp.ClientTimeout(total=15),
        ) as resp:
            if resp.status != 200:
                text = await resp.text()
                print(f"[bot] internal/mii: HTTP {resp.status} for {pnid}: {text}", flush=True)
                return None, None
            data = await resp.json()
        b64 = data["data"]
        b64 += "=" * (-len(b64) % 4)
        mii_bytes = base64.b64decode(b64)
        mii_name = data.get("name") or pnid
        return mii_bytes, mii_name
    except Exception as e:
        print(f"[bot] internal/mii error for {pnid}: {e}", flush=True)
        return None, None


async def _render_mii_png(mii_bytes: bytes, session: ClientSession):
    """Render FFLStoreData to a PNG via the same backend /mii uses. Returns (img_data, error) —
    exactly one of which is set."""
    mii_b64 = base64.urlsafe_b64encode(mii_bytes).decode().rstrip("=")
    render_url = f"https://mii-unsecure.ariankordi.net/miis/image.png?data={mii_b64}&width=2048&type=face&api_id=1"
    async with session.get(render_url) as resp:
        if resp.status != 200:
            return None, f"Mii render API returned HTTP {resp.status}."
        return await resp.read(), None


@bot.tree.command(guild=_guild, name="mii", description="Render a Mii as a 2048×2048 image")
@app_commands.describe(pnid="PNID to render (leave blank to use your linked PNID)")
async def mii_cmd(interaction: discord.Interaction, pnid: str = ""):
    await interaction.response.defer()
    pnid = pnid.strip()

    with db_conn() as conn:
        with conn.cursor() as cur:
            if not pnid:
                cur.execute("SELECT username FROM wii_devices WHERE discord_id = %s", (str(interaction.user.id),))
                row = cur.fetchone()
                if not row:
                    await interaction.followup.send("❌ No PNID linked. Use `/link_pnid` or provide a PNID.", ephemeral=True)
                    return
                pnid = row[0]

            # Local user — user_settings
            cur.execute("SELECT mii_data, mii_name FROM user_settings WHERE nnid = %s AND mii_data IS NOT NULL", (pnid,))
            row = cur.fetchone()
            if not row:
                # Pretendo friend in the cache
                cur.execute(
                    "SELECT mii_data, mii_name FROM pretendo_friends WHERE friend_nnid = %s AND mii_data IS NOT NULL LIMIT 1",
                    (pnid,),
                )
                row = cur.fetchone()
            if not row:
                # Generic mii_cache (populated by account-proxy after PRUDP or REST fetch)
                cur.execute(
                    "SELECT mii_data, mii_name FROM mii_cache WHERE pnid = %s AND mii_data IS NOT NULL",
                    (pnid,),
                )
                row = cur.fetchone()

    mii_name = pnid
    mii_bytes = bytes(row[0]) if (row and row[0]) else None
    if mii_bytes:
        mii_name = row[1] or pnid

    async with ClientSession() as session:
        if not mii_bytes:
            # Fetch from Pretendo account server as fallback
            mii_bytes, fetched_name = await _fetch_pretendo_mii(pnid, session)
            if mii_bytes:
                mii_name = fetched_name or pnid

        if not mii_bytes:
            await interaction.followup.send(
                f"❌ No Mii data found for **{pnid}**.\n"
                f"-# This PNID hasn't been discovered by our server yet. "
                f"Ask them to connect to Revivetendo at least once.",
                ephemeral=True,
            )
            return

        img_data, render_err = await _render_mii_png(mii_bytes, session)
        if render_err:
            await interaction.followup.send(f"❌ {render_err}", ephemeral=True)
            return

    embed = discord.Embed(title=mii_name, color=0x7c3aed)
    embed.set_image(url="attachment://mii.png")
    embed.set_footer(text=f"PNID: {pnid}")
    await interaction.followup.send(embed=embed, file=discord.File(io.BytesIO(img_data), filename="mii.png"))


# ──────────────────────────────────────────────
# Mii of the Day — daily post at 00:00 UTC
# ──────────────────────────────────────────────

def _pick_random_mii():
    """Pick one random (pnid, mii_name, mii_bytes) from every PNID we have Mii data for,
    across all three sources /mii falls back through. Returns None if none found."""
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute("""
                SELECT DISTINCT ON (pnid) pnid, mii_name, mii_data FROM (
                    SELECT nnid AS pnid, mii_name, mii_data FROM user_settings
                        WHERE mii_data IS NOT NULL AND nnid <> ''
                    UNION ALL
                    SELECT friend_nnid AS pnid, mii_name, mii_data FROM pretendo_friends
                        WHERE mii_data IS NOT NULL AND friend_nnid <> ''
                    UNION ALL
                    SELECT pnid, mii_name, mii_data FROM mii_cache
                        WHERE mii_data IS NOT NULL AND pnid <> ''
                ) AS all_miis
                ORDER BY pnid
            """)
            rows = cur.fetchall()
    if not rows:
        return None
    pnid, mii_name, mii_data = random.choice(rows)
    return pnid, (mii_name or pnid), bytes(mii_data)


@tasks.loop(time=time(hour=0, minute=0, tzinfo=timezone.utc))
async def mii_of_the_day():
    if not MII_OF_DAY_CHANNEL_ID:
        return
    channel = bot.get_channel(MII_OF_DAY_CHANNEL_ID)
    if channel is None:
        try:
            channel = await bot.fetch_channel(MII_OF_DAY_CHANNEL_ID)
        except discord.HTTPException as e:
            print(f"[bot] mii_of_the_day: channel {MII_OF_DAY_CHANNEL_ID} not found: {e}", flush=True)
            return

    picked = _pick_random_mii()
    if not picked:
        print("[bot] mii_of_the_day: no PNIDs with Mii data found, skipping", flush=True)
        return
    pnid, mii_name, mii_bytes = picked

    async with ClientSession() as session:
        img_data, render_err = await _render_mii_png(mii_bytes, session)
    if render_err:
        print(f"[bot] mii_of_the_day: render failed for {pnid}: {render_err}", flush=True)
        return

    embed = discord.Embed(title="Mii of the Day", description=f"**{mii_name}**", color=0x7c3aed, timestamp=_now())
    embed.set_image(url="attachment://mii.png")
    embed.set_footer(text=f"PNID: {pnid}")
    await channel.send(embed=embed, file=discord.File(io.BytesIO(img_data), filename="mii.png"))
    print(f"[bot] mii_of_the_day: posted {pnid}", flush=True)


def _mii_embed(pnid: str, mii_name: str) -> discord.Embed:
    embed = discord.Embed(title=mii_name, color=0x7c3aed)
    embed.set_image(url="attachment://mii.png")
    embed.set_footer(text=f"PNID: {pnid}")
    return embed


@bot.tree.command(guild=_guild, name="mii_slideshow", description="Show a new random Mii every 5 seconds for a while")
@app_commands.describe(minutes="How many minutes to run for (default 1, max 5)")
async def mii_slideshow(interaction: discord.Interaction, minutes: app_commands.Range[float, 0.5, 5.0] = 1.0):
    await interaction.response.defer()

    picked = _pick_random_mii()
    if not picked:
        await interaction.followup.send("❌ No Mii data found yet.", ephemeral=True)
        return
    pnid, mii_name, mii_bytes = picked

    async with ClientSession() as session:
        img_data, render_err = await _render_mii_png(mii_bytes, session)
    if render_err:
        await interaction.followup.send(f"❌ {render_err}", ephemeral=True)
        return

    message = await interaction.followup.send(
        embed=_mii_embed(pnid, mii_name),
        file=discord.File(io.BytesIO(img_data), filename="mii.png"),
        wait=True,
    )

    end_at = _time.monotonic() + minutes * 60
    async with ClientSession() as session:
        while _time.monotonic() < end_at:
            await asyncio.sleep(5)
            picked = _pick_random_mii()
            if not picked:
                continue
            pnid, mii_name, mii_bytes = picked
            img_data, render_err = await _render_mii_png(mii_bytes, session)
            if render_err:
                continue
            try:
                await message.edit(embed=_mii_embed(pnid, mii_name), attachments=[discord.File(io.BytesIO(img_data), filename="mii.png")])
            except discord.HTTPException as e:
                print(f"[bot] mii_slideshow: edit failed, stopping early: {e}", flush=True)
                break


@bot.tree.command(guild=_guild, name="startmiitv", description="Launch the Mii TV slideshow Activity in your voice channel")
async def startmiitv(interaction: discord.Interaction):
    member = interaction.user
    if not isinstance(member, discord.Member) or member.voice is None or member.voice.channel is None:
        await interaction.response.send_message("❌ You need to be in a voice channel first.", ephemeral=True)
        return

    client_id = os.environ.get("DISCORD_ACTIVITY_CLIENT_ID")
    if not client_id:
        await interaction.response.send_message("❌ Mii TV isn't set up yet (missing DISCORD_ACTIVITY_CLIENT_ID).", ephemeral=True)
        return

    try:
        invite = await member.voice.channel.create_invite(
            target_type=discord.InviteTarget.embedded_application,
            target_application_id=int(client_id),
            max_age=300,
        )
    except (discord.HTTPException, ValueError) as e:
        await interaction.response.send_message(f"❌ Couldn't start Mii TV: {e}", ephemeral=True)
        return

    await interaction.response.send_message(f"📺 **Mii TV** is starting in **{member.voice.channel.name}** — click to join: {invite.url}")


def _synthesize_tts_wav(text: str) -> str:
    fd, path = tempfile.mkstemp(suffix=".wav", prefix="tts-")
    os.close(fd)
    with wave.open(path, "wb") as wf:
        piper_voice.synthesize_wav(text, wf)
    return path


@bot.tree.command(guild=_guild, name="revivetendo_tts", description="Speak text out loud in your voice channel (local TTS)")
@app_commands.describe(text="What to say (max 500 characters)")
async def revivetendo_tts(interaction: discord.Interaction, text: app_commands.Range[str, 1, 500]):
    vc = interaction.guild.voice_client if interaction.guild else None
    if vc is None:
        await interaction.response.send_message("❌ I'm not in a voice channel — use `/joinvc` first.", ephemeral=True)
        return

    await interaction.response.defer(ephemeral=True)

    try:
        wav_path = await asyncio.to_thread(_synthesize_tts_wav, text)
    except Exception as e:
        print(f"[bot] revivetendo_tts: synthesis failed: {e}", flush=True)
        await interaction.followup.send("❌ TTS synthesis failed.", ephemeral=True)
        return

    try:
        mixer = _get_or_create_mixer(vc)
        source = discord.FFmpegPCMAudio(wav_path)
        playback_done = asyncio.Event()
        mixer.add_source(_NotifyingSource(source, lambda: bot.loop.call_soon_threadsafe(playback_done.set)))
        await playback_done.wait()
    finally:
        os.remove(wav_path)

    await interaction.followup.send("🗣️ Said it.", ephemeral=True)


# ──────────────────────────────────────────────
# Audio mixer — shared by /joinvc's clip playback and /revivetendo_tts so they
# can't stomp on each other. discord.py's VoiceClient only ever allows ONE
# AudioSource to be handed to vc.play() at a time (a second call while
# something's already playing raises ClientException, or worse - if that
# error was allowed to kill a coroutine mid-flow, it could leave the other
# feature's asyncio.Event/cleanup dangling). Instead, each guild gets exactly
# one _GuildMixer, passed to vc.play() once when the bot connects; everything
# else (joinvc clips, TTS lines) is added to it as a sub-source and mixed by
# summing PCM frames, so multiple things can genuinely play at once.
# ──────────────────────────────────────────────

class _GuildMixer(discord.AudioSource):
    FRAME_SIZE = discord.opus.Encoder.FRAME_SIZE  # bytes per 20ms of 48kHz stereo s16le PCM
    SILENCE_TIMEOUT = 3.0  # seconds of nothing to mix before we actually stop sending packets

    def __init__(self, vc: discord.VoiceClient):
        self._vc = vc
        self._lock = threading.Lock()
        self._sources: list[discord.AudioSource] = []
        self._idle_since: float | None = None

    def add_source(self, source: discord.AudioSource):
        with self._lock:
            self._sources.append(source)
            self._idle_since = None
        if self._vc.is_paused():
            self._vc.resume()

    def read(self) -> bytes:
        with self._lock:
            sources = list(self._sources)

        mixed = array.array("h", bytes(self.FRAME_SIZE))
        finished = []
        any_data = False
        for src in sources:
            try:
                chunk = src.read()
            except Exception as e:
                print(f"[bot] mixer: sub-source read failed, dropping it: {e}", flush=True)
                chunk = b""
            if not chunk:
                finished.append(src)
                continue
            any_data = True
            if len(chunk) < self.FRAME_SIZE:
                chunk += b"\x00" * (self.FRAME_SIZE - len(chunk))
            samples = array.array("h")
            samples.frombytes(chunk[: self.FRAME_SIZE])
            for i, s in enumerate(samples):
                mixed[i] = max(-32768, min(32767, mixed[i] + s))

        if finished:
            with self._lock:
                for f in finished:
                    if f in self._sources:
                        self._sources.remove(f)
            for f in finished:
                try:
                    f.cleanup()
                except Exception:
                    pass

        if any_data:
            with self._lock:
                self._idle_since = None
            return mixed.tobytes()

        # Nothing to mix this frame. After SILENCE_TIMEOUT seconds of that,
        # actually pause the underlying player instead of forever streaming
        # silence - vc.pause() sends the standard 5-frame comfort-noise burst
        # itself, then genuinely stops sending packets until add_source()
        # resumes it. (Returning b"" here instead would tell discord.py the
        # *mixer itself* is exhausted and end the voice stream permanently.)
        now = _time.monotonic()
        with self._lock:
            if self._idle_since is None:
                self._idle_since = now
            idle_for = now - self._idle_since
        if idle_for >= self.SILENCE_TIMEOUT and not self._vc.is_paused():
            self._vc.pause()
        return b"\x00" * self.FRAME_SIZE

    def is_opus(self) -> bool:
        return False

    def cleanup(self):
        with self._lock:
            sources, self._sources = self._sources, []
        for s in sources:
            try:
                s.cleanup()
            except Exception:
                pass


class _NotifyingSource(discord.AudioSource):
    """Wraps a sub-source so the mixer can host it while still letting the
    caller know when it's done playing (mixer.add_source has no built-in
    equivalent of vc.play()'s `after` callback)."""

    def __init__(self, source: discord.AudioSource, on_done):
        self._source = source
        self._on_done = on_done
        self._notified = False

    def read(self) -> bytes:
        chunk = self._source.read()
        if not chunk and not self._notified:
            self._notified = True
            self._on_done()
        return chunk

    def is_opus(self) -> bool:
        return self._source.is_opus()

    def cleanup(self):
        self._source.cleanup()


def _get_or_create_mixer(vc: discord.VoiceClient) -> _GuildMixer:
    mixer = getattr(vc, "mixer", None)
    if mixer is None:
        mixer = _GuildMixer(vc)
        vc.mixer = mixer
        vc.play(mixer)
    return mixer


# ──────────────────────────────────────────────
# Voice clip playback — /joinvc
# ──────────────────────────────────────────────

_active_voice_sessions: dict[int, asyncio.Event] = {}  # guild ID -> leave-requested event, for guilds /joinvc is currently active in


class _VoiceRecordSession:
    """Captures up to 15s of PCM from the first non-bot member to speak in the VC,
    stopping early after a short silence gap. on_audio() runs on voice_recv's
    background packet-router thread (not the event loop), so state is guarded by a
    lock and completion is signalled back onto the bot's loop via call_soon_threadsafe.

    Real Discord clients now always negotiate DAVE end-to-end voice encryption, so the
    bytes voice_recv hands us (VoiceData.opus, with decode=False) are still
    DAVE-encrypted, not plain Opus — voice_recv predates DAVE and knows nothing about
    it. We unwrap that ourselves via the `davey` session discord.py already maintains
    for sending (vc._connection.dave_session), which exposes the matching decrypt()
    primitive, then Opus-decode the result ourselves. Bots/other non-E2EE participants
    use DAVE's "passthrough" mode instead (plain Opus already) — dave_session.can_passthrough
    tells us which path a given sender is on."""

    SAMPLE_RATE = 48000
    CHANNELS = 2
    SAMPLE_WIDTH = 2
    MAX_SECONDS = 15.0
    SILENCE_GAP_SECONDS = 1.0
    MIN_SECONDS_BEFORE_SILENCE_STOP = 0.5

    def __init__(self, loop: asyncio.AbstractEventLoop, vc: "voice_recv.VoiceRecvClient"):
        self._loop = loop
        self._lock = threading.Lock()
        self._buffer = bytearray()
        self._target_id: int | None = None
        self._target_name: str | None = None
        self._last_packet_at: float | None = None
        self._done = asyncio.Event()
        self.result: tuple[bytes, str] | None = None
        self._dave = vc._connection.dave_session
        self._decoder = discord.opus.Decoder()
        # Diagnostics only — see run()'s periodic summary print.
        self.total_packets = 0
        self.none_user_packets = 0
        self.bot_packets = 0
        self.dave_errors = 0
        self.other_user_ids: set = set()

    def _max_bytes(self) -> int:
        return int(self.MAX_SECONDS * self.SAMPLE_RATE * self.CHANNELS * self.SAMPLE_WIDTH)

    def _min_bytes(self) -> int:
        return int(self.MIN_SECONDS_BEFORE_SILENCE_STOP * self.SAMPLE_RATE * self.CHANNELS * self.SAMPLE_WIDTH)

    def on_audio(self, user, data):
        """Sink callback for voice_recv.BasicSink — ignores unresolved SSRCs and bots,
        locks onto the first other speaker, and ignores anyone else until done."""
        self.total_packets += 1
        if user is None:
            self.none_user_packets += 1
            return
        if getattr(user, "bot", False):
            self.bot_packets += 1
            return
        self.other_user_ids.add(user.id)
        with self._lock:
            if self._done.is_set():
                return
            if self._target_id is None:
                self._target_id = user.id
                self._target_name = getattr(user, "display_name", None) or str(user)
            elif user.id != self._target_id:
                return

        try:
            if self._dave is not None and not self._dave.can_passthrough(user.id):
                opus_bytes = self._dave.decrypt(user.id, davey.MediaType.audio, data.opus)
            else:
                opus_bytes = data.opus
            pcm = self._decoder.decode(opus_bytes, fec=False)
        except Exception:
            # Expected occasionally (e.g. a stray frame during a DAVE key rotation) —
            # just drop this frame and keep going.
            self.dave_errors += 1
            return

        should_finish = False
        with self._lock:
            if self._done.is_set():
                return
            self._buffer.extend(pcm)
            self._last_packet_at = _time.monotonic()
            if len(self._buffer) >= self._max_bytes():
                should_finish = True
        if should_finish:
            self._loop.call_soon_threadsafe(self._finish)

    def _finish(self):
        if self._done.is_set():
            return
        with self._lock:
            if self._buffer:
                self.result = (bytes(self._buffer), self._target_name or "someone")
            buffered = len(self._buffer)
        print(
            f"[joinvc] captured {buffered} bytes from {self._target_name!r} "
            f"(packets: total={self.total_packets} bot={self.bot_packets} dropped={self.dave_errors})",
            flush=True,
        )
        self._done.set()

    async def run(self) -> "tuple[bytes, str] | None":
        """Waits indefinitely for someone to speak and finish speaking. The caller
        (joinvc's loop) is responsible for cancelling this if it needs to stop for
        another reason (empty channel, /leavevc)."""
        while not self._done.is_set():
            try:
                await asyncio.wait_for(self._done.wait(), timeout=0.3)
            except asyncio.TimeoutError:
                pass
            with self._lock:
                has_target = self._target_id is not None and self._last_packet_at is not None
                silent_for = (_time.monotonic() - self._last_packet_at) if has_target else 0.0
                enough_captured = len(self._buffer) >= self._min_bytes()

            if has_target and silent_for >= self.SILENCE_GAP_SECONDS and enough_captured:
                self._finish()
                break
        return self.result


def _write_pcm_to_wav(pcm: bytes) -> str:
    fd, path = tempfile.mkstemp(suffix=".wav", prefix="joinvc-")
    os.close(fd)
    with wave.open(path, "wb") as wf:
        wf.setnchannels(_VoiceRecordSession.CHANNELS)
        wf.setsampwidth(_VoiceRecordSession.SAMPLE_WIDTH)
        wf.setframerate(_VoiceRecordSession.SAMPLE_RATE)
        wf.writeframes(pcm)
    return path


EMPTY_CHANNEL_TIMEOUT_SECONDS = 45.0


async def _disconnect_when_empty(vc: "voice_recv.VoiceRecvClient", timeout: float):
    """Returns once the bot's voice channel has had zero non-bot members for `timeout`
    seconds straight. Runs for as long as the caller races it against."""
    empty_since = None
    while vc.is_connected():
        await asyncio.sleep(2)
        if not any(not m.bot for m in vc.channel.members):
            empty_since = empty_since or _time.monotonic()
            if _time.monotonic() - empty_since >= timeout:
                return
        else:
            empty_since = None


async def _record_and_playback_once(vc: "voice_recv.VoiceRecvClient", interaction: discord.Interaction):
    """One listen-record-playback cycle. Waits indefinitely for a speaker; joinvc's
    loop cancels this (between cycles it's safe to cancel) to stop the whole session."""
    session = _VoiceRecordSession(bot.loop, vc)
    vc.listen(voice_recv.BasicSink(session.on_audio, decode=False))
    result = await session.run()
    vc.stop_listening()

    if result is None:
        return

    pcm, _ = result

    wav_path = await asyncio.to_thread(_write_pcm_to_wav, pcm)
    try:
        mixer = _get_or_create_mixer(vc)
        source = discord.FFmpegPCMAudio(wav_path, options="-filter:a asetrate=48000*1.5,aresample=48000")
        playback_done = asyncio.Event()
        mixer.add_source(_NotifyingSource(source, lambda: bot.loop.call_soon_threadsafe(playback_done.set)))
        await playback_done.wait()
    finally:
        os.remove(wav_path)


@bot.tree.command(guild=_guild, name="joinvc", description="Join your VC and keep playing back sped-up clips of whoever talks (use /leavevc to stop)")
async def joinvc(interaction: discord.Interaction):
    member = interaction.user
    if not isinstance(member, discord.Member) or member.voice is None or member.voice.channel is None:
        await interaction.response.send_message("❌ You need to be in a voice channel first.", ephemeral=True)
        return

    guild_id = interaction.guild_id
    if guild_id in _active_voice_sessions:
        await interaction.response.send_message("🎙️ Already active in a voice channel in this server — use `/leavevc` first.", ephemeral=True)
        return

    channel = member.voice.channel
    leave_event = asyncio.Event()
    _active_voice_sessions[guild_id] = leave_event
    try:
        await interaction.response.send_message(
            f"🎙️ Joining **{channel.name}** — say something and I'll play it back sped up! "
            f"I'll keep listening until the channel's empty for {int(EMPTY_CHANNEL_TIMEOUT_SECONDS)}s or someone runs `/leavevc`."
        )

        try:
            vc = await channel.connect(cls=voice_recv.VoiceRecvClient)
        except Exception as e:
            await interaction.followup.send(f"❌ Couldn't join the voice channel: {e}")
            return

        _get_or_create_mixer(vc)

        dave_version = getattr(vc._connection, "dave_protocol_version", None)
        print(
            f"[joinvc] connected to {channel.name!r} (guild={guild_id}) mode={vc.mode!r} "
            f"dave_protocol_version={dave_version} members={[ (m.name, m.bot) for m in channel.members ]}",
            flush=True,
        )

        try:
            empty_task = asyncio.ensure_future(_disconnect_when_empty(vc, EMPTY_CHANNEL_TIMEOUT_SECONDS))
            leave_task = asyncio.ensure_future(leave_event.wait())
            stop_reason = "leave"
            while True:
                record_task = asyncio.ensure_future(_record_and_playback_once(vc, interaction))
                done, _pending = await asyncio.wait({record_task, empty_task, leave_task}, return_when=asyncio.FIRST_COMPLETED)

                if record_task in done:
                    exc = record_task.exception()
                    if exc:
                        raise exc
                    continue  # played a clip - go listen for the next speaker

                record_task.cancel()
                try:
                    await record_task
                except (asyncio.CancelledError, Exception):
                    pass
                stop_reason = "empty" if empty_task in done else "leave"
                break

            for t in (empty_task, leave_task):
                if not t.done():
                    t.cancel()

            if stop_reason == "empty":
                await interaction.followup.send(f"👋 Nobody's been in the channel for {int(EMPTY_CHANNEL_TIMEOUT_SECONDS)}s — leaving.")
            else:
                await interaction.followup.send("👋 Leaving now.")
        finally:
            await vc.disconnect()
    finally:
        _active_voice_sessions.pop(guild_id, None)


@bot.tree.command(guild=_guild, name="leavevc", description="Make the bot leave the voice channel it's currently in")
async def leavevc(interaction: discord.Interaction):
    guild_id = interaction.guild_id
    leave_event = _active_voice_sessions.get(guild_id)
    if leave_event is not None:
        leave_event.set()
        await interaction.response.send_message("👋 Leaving now.")
        return

    vc = interaction.guild.voice_client if interaction.guild else None
    if vc is not None:
        await vc.disconnect(force=True)
        await interaction.response.send_message("👋 Leaving now.")
        return

    await interaction.response.send_message("❌ I'm not in a voice channel right now.", ephemeral=True)


@bot.tree.command(guild=_guild, name="whois", description="Look up the linked PNID for a Discord user")
@app_commands.describe(user="The Discord user to look up")
@is_moderator()
async def whois(interaction: discord.Interaction, user: discord.Member):
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT username FROM wii_devices WHERE discord_id = %s", (str(user.id),))
            row = cur.fetchone()
    if row:
        await interaction.response.send_message(f"🎮 {user.mention} → PNID **{row[0]}**", ephemeral=True)
    else:
        await interaction.response.send_message(f"❓ {user.mention} has no linked PNID.", ephemeral=True)

# ──────────────────────────────────────────────
# Slash commands — moderation
# ──────────────────────────────────────────────

@bot.tree.command(guild=_guild, name="ban", description="Ban a member from the server")
@app_commands.describe(user="Member to ban", reason="Reason for the ban", delete_days="Days of messages to delete (0–7)")
@is_moderator()
async def ban_cmd(interaction: discord.Interaction, user: discord.Member, reason: str = "No reason provided", delete_days: int = 0):
    try:
        await user.send(f"🔨 You have been **banned** from **{interaction.guild.name}**.\nReason: {reason}")
    except discord.Forbidden:
        pass
    await user.ban(reason=f"{interaction.user}: {reason}", delete_message_days=max(0, min(7, delete_days)))
    embed = discord.Embed(title="Member Banned", color=0xED4245, timestamp=_now())
    embed.add_field(name="User",      value=f"{user} ({user.id})")
    embed.add_field(name="Moderator", value=str(interaction.user))
    embed.add_field(name="Reason",    value=reason, inline=False)
    await _mod_log(embed)
    await interaction.response.send_message(f"🔨 Banned **{user}**.", ephemeral=True)


@bot.tree.command(guild=_guild, name="kick", description="Kick a member from the server")
@app_commands.describe(user="Member to kick", reason="Reason for the kick")
@is_moderator()
async def kick_cmd(interaction: discord.Interaction, user: discord.Member, reason: str = "No reason provided"):
    try:
        await user.send(f"👢 You have been **kicked** from **{interaction.guild.name}**.\nReason: {reason}")
    except discord.Forbidden:
        pass
    await user.kick(reason=f"{interaction.user}: {reason}")
    embed = discord.Embed(title="Member Kicked", color=0xE67E22, timestamp=_now())
    embed.add_field(name="User",      value=f"{user} ({user.id})")
    embed.add_field(name="Moderator", value=str(interaction.user))
    embed.add_field(name="Reason",    value=reason, inline=False)
    await _mod_log(embed)
    await interaction.response.send_message(f"👢 Kicked **{user}**.", ephemeral=True)


@bot.tree.command(guild=_guild, name="timeout", description="Time out a member")
@app_commands.describe(user="Member to time out", minutes="Duration in minutes (max 40320 = 28 days)", reason="Reason")
@is_moderator()
async def timeout_cmd(interaction: discord.Interaction, user: discord.Member, minutes: int, reason: str = "No reason provided"):
    until = _now() + timedelta(minutes=max(1, min(40320, minutes)))
    await user.timeout(until, reason=f"{interaction.user}: {reason}")
    embed = discord.Embed(title="Member Timed Out", color=0xFEE75C, timestamp=_now())
    embed.add_field(name="User",      value=f"{user} ({user.id})")
    embed.add_field(name="Moderator", value=str(interaction.user))
    embed.add_field(name="Duration",  value=f"{minutes} minute(s)")
    embed.add_field(name="Reason",    value=reason, inline=False)
    await _mod_log(embed)
    await interaction.response.send_message(f"⏱️ Timed out **{user}** for {minutes} minute(s).", ephemeral=True)


@bot.tree.command(guild=_guild, name="warn", description="Issue a warning to a member")
@app_commands.describe(user="Member to warn", reason="Reason for the warning")
@is_moderator()
async def warn_cmd(interaction: discord.Interaction, user: discord.Member, reason: str):
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(
                "INSERT INTO discord_warns (discord_id, moderator_id, reason) VALUES (%s, %s, %s)",
                (str(user.id), str(interaction.user.id), reason),
            )
            cur.execute("SELECT COUNT(*) FROM discord_warns WHERE discord_id = %s", (str(user.id),))
            total = cur.fetchone()[0]
        conn.commit()
    warn_msg = f"⚠️ You received a **warning** in **{interaction.guild.name}** (#{total} total).\nReason: {reason}"
    try:
        await user.send(warn_msg)
    except (discord.Forbidden, aiohttp.ClientPayloadError):
        if RING_FALLBACK_CH_ID and (ch := bot.get_channel(RING_FALLBACK_CH_ID)):
            await ch.send(f"{user.mention} {warn_msg}")
    embed = discord.Embed(title=f"Member Warned (#{total} total)", color=0xFEE75C, timestamp=_now())
    embed.add_field(name="User",      value=f"{user} ({user.id})")
    embed.add_field(name="Moderator", value=str(interaction.user))
    embed.add_field(name="Reason",    value=reason, inline=False)
    await _mod_log(embed)
    await interaction.response.send_message(f"⚠️ Warned **{user}** (warning #{total}).", ephemeral=True)


@bot.tree.command(guild=_guild, name="warns", description="List warnings for a member")
@app_commands.describe(user="Member to check")
@is_moderator()
async def warns_cmd(interaction: discord.Interaction, user: discord.Member):
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT reason, created_at FROM discord_warns WHERE discord_id = %s ORDER BY created_at DESC LIMIT 10",
                (str(user.id),),
            )
            rows = cur.fetchall()
    if not rows:
        await interaction.response.send_message(f"✅ {user.mention} has no warnings.", ephemeral=True)
        return
    embed = discord.Embed(title=f"Warnings for {user}", color=0xFEE75C, timestamp=_now())
    for reason, created_at in rows:
        embed.add_field(name=discord.utils.format_dt(created_at, "f"), value=reason, inline=False)
    await interaction.response.send_message(embed=embed, ephemeral=True)


@bot.tree.command(guild=_guild, name="clear", description="Delete recent messages in this channel")
@app_commands.describe(count="Number of messages to delete (1–100)")
@is_moderator()
async def clear_cmd(interaction: discord.Interaction, count: int):
    count = max(1, min(100, count))
    await interaction.response.defer(ephemeral=True)
    deleted = await interaction.channel.purge(limit=count)
    await interaction.followup.send(f"🗑️ Deleted {len(deleted)} message(s).", ephemeral=True)

# ──────────────────────────────────────────────
# Ring notification HTTP endpoint (called by friends-nex)
# ──────────────────────────────────────────────

async def _handle_ring(request):
    try:
        data = await request.json()
        target_pid = str(data.get("target_pid", ""))
        caller_pid = str(data.get("caller_pid", ""))

        with db_conn() as conn:
            with conn.cursor() as cur:
                cur.execute("""
                    SELECT wd.discord_id,
                           COALESCE(
                               NULLIF(pf.friend_nnid,''),
                               NULLIF(us2.nnid,''),
                               na2.username
                           )
                    FROM user_settings us
                    JOIN wii_devices wd ON wd.username = us.nnid
                    LEFT JOIN nex_accounts na2 ON na2.pid = %s
                    LEFT JOIN user_settings us2 ON us2.pid = %s
                    LEFT JOIN pretendo_friends pf ON pf.owner_pid = us.pid AND pf.friend_pid = %s
                    WHERE us.pid = %s AND wd.discord_id IS NOT NULL
                """, (caller_pid, caller_pid, caller_pid, target_pid))
                row = cur.fetchone()

        if not row:
            return web.Response(text="no discord link")

        discord_id, caller_pnid = int(row[0]), row[1] or f"PID {caller_pid}"
        msg = (
            f"📞 **{caller_pnid}** is calling you on **WiiU Chat** via Revivetendo!\n"
            "Open the WiiU Chat app on your Wii U to answer."
        )
        user = await bot.fetch_user(discord_id)
        try:
            await user.send(msg)
            print(f"[bot] ring DM sent: caller={caller_pnid} → discord_id={discord_id}", flush=True)
        except (discord.Forbidden, aiohttp.ClientPayloadError):
            if RING_FALLBACK_CH_ID and (ch := bot.get_channel(RING_FALLBACK_CH_ID)):
                await ch.send(f"<@{discord_id}> {msg}")
                print(f"[bot] ring fallback channel: caller={caller_pnid} → discord_id={discord_id}", flush=True)
            else:
                print(f"[bot] ring DM blocked and no fallback channel configured for discord_id={discord_id}", flush=True)
        return web.Response(text="ok")
    except Exception as e:
        import traceback
        print(f"[bot] ring handler error: {e}\n{traceback.format_exc()}", flush=True)
        return web.Response(text=str(e), status=500)


async def _start_http():
    app = web.Application()
    app.router.add_post("/ring", _handle_ring)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", RING_PORT)
    await site.start()
    print(f"[bot] ring HTTP on 127.0.0.1:{RING_PORT}", flush=True)

# ──────────────────────────────────────────────
# Entry point
# ──────────────────────────────────────────────

async def main():
    db_init()
    async with bot:
        await _start_http()
        await bot.start(TOKEN)

asyncio.run(main())
