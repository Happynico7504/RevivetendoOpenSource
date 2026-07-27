#!/usr/bin/env python3
"""Revivetendo Bot — community Discord bot for the Revivetendo Pretendo Bridge."""

import asyncio
import base64
import io
import os
from datetime import datetime, timedelta, timezone

import discord
from discord import app_commands
from discord.ext import commands
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
MOD_LOG_ID          = int(os.environ.get("DISCORD_MOD_LOG_CHANNEL_ID", "0"))
ACTIVITY_LOG_ID     = int(os.environ.get("DISCORD_ACTIVITY_LOG_CHANNEL_ID", "0"))
RING_FALLBACK_CH_ID = int(os.environ.get("DISCORD_RING_FALLBACK_CHANNEL_ID", "0"))
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

def _now():
    return datetime.now(timezone.utc)

async def _mod_log(embed: discord.Embed):
    if MOD_LOG_ID and (ch := bot.get_channel(MOD_LOG_ID)):
        await ch.send(embed=embed)

async def _activity_log(embed: discord.Embed):
    if ACTIVITY_LOG_ID and (ch := bot.get_channel(ACTIVITY_LOG_ID)):
        await ch.send(embed=embed)

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

        mii_b64 = base64.urlsafe_b64encode(mii_bytes).decode().rstrip("=")
        render_url = f"https://mii-unsecure.ariankordi.net/miis/image.png?data={mii_b64}&width=2048&type=face&api_id=1"
        async with session.get(render_url) as resp:
            if resp.status != 200:
                await interaction.followup.send(f"❌ Mii render API returned HTTP {resp.status}.", ephemeral=True)
                return
            img_data = await resp.read()

    embed = discord.Embed(title=mii_name, color=0x7c3aed)
    embed.set_image(url="attachment://mii.png")
    embed.set_footer(text=f"PNID: {pnid}")
    await interaction.followup.send(embed=embed, file=discord.File(io.BytesIO(img_data), filename="mii.png"))


@bot.tree.command(guild=_guild, name="whois", description="Look up the linked PNID for a Discord user")
@app_commands.describe(user="The Discord user to look up")
@app_commands.default_permissions(manage_messages=True)
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
@app_commands.default_permissions(ban_members=True)
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
@app_commands.default_permissions(kick_members=True)
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
@app_commands.default_permissions(moderate_members=True)
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
@app_commands.default_permissions(manage_messages=True)
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
@app_commands.default_permissions(manage_messages=True)
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
@app_commands.default_permissions(manage_messages=True)
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
