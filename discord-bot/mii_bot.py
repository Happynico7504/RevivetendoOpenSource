#!/usr/bin/env python3
"""Revivetendo Mii Bot — user-installable /revivetendo_mii command."""

import asyncio
import base64
import io
import os

import discord
from discord import app_commands
from discord.ext import commands
import psycopg2
import aiohttp
from aiohttp import ClientSession

_env_path = os.path.join(os.path.dirname(__file__), ".env")
if os.path.exists(_env_path):
    with open(_env_path) as _f:
        for _line in _f:
            _line = _line.strip()
            if _line and not _line.startswith("#") and "=" in _line:
                _k, _v = _line.split("=", 1)
                os.environ.setdefault(_k.strip(), _v.strip())

TOKEN  = os.environ["MII_BOT_TOKEN"]
DB_URL = os.environ.get("DATABASE_URL", "postgres://postgres:wiiu@localhost:5432/wiiuchat?sslmode=disable")

_ACCOUNT_PROXY_MII_URL = "http://127.0.0.1:9191/internal/mii"

def db_conn():
    return psycopg2.connect(DB_URL)

intents = discord.Intents.default()
bot = commands.Bot(command_prefix="!", intents=intents)


async def _fetch_pretendo_mii(pnid: str, session: ClientSession):
    try:
        async with session.get(
            _ACCOUNT_PROXY_MII_URL,
            params={"pnid": pnid},
            timeout=aiohttp.ClientTimeout(total=15),
        ) as resp:
            if resp.status != 200:
                print(f"[mii-bot] internal/mii: HTTP {resp.status} for {pnid}", flush=True)
                return None, None
            data = await resp.json()
        b64 = data["data"]
        b64 += "=" * (-len(b64) % 4)
        return base64.b64decode(b64), data.get("name") or pnid
    except Exception as e:
        print(f"[mii-bot] internal/mii error for {pnid}: {e}", flush=True)
        return None, None


@bot.tree.command(name="revivetendo_mii", description="Render a Revivetendo Mii as a 2048×2048 image")
@app_commands.describe(pnid="PNID to render (leave blank to use your linked PNID)")
@app_commands.allowed_installs(guilds=True, users=True)
@app_commands.allowed_contexts(guilds=True, dms=True, private_channels=True)
async def revivetendo_mii_cmd(interaction: discord.Interaction, pnid: str = ""):
    await interaction.response.defer()
    pnid = pnid.strip()

    with db_conn() as conn:
        with conn.cursor() as cur:
            if not pnid:
                cur.execute("SELECT username FROM wii_devices WHERE discord_id = %s", (str(interaction.user.id),))
                row = cur.fetchone()
                if not row:
                    await interaction.followup.send(
                        "❌ No PNID linked. Use `/link_pnid` in the Revivetendo Discord or provide a PNID.",
                        ephemeral=True,
                    )
                    return
                pnid = row[0]

            cur.execute("SELECT mii_data, mii_name FROM user_settings WHERE nnid = %s AND mii_data IS NOT NULL", (pnid,))
            row = cur.fetchone()
            if not row:
                cur.execute(
                    "SELECT mii_data, mii_name FROM pretendo_friends WHERE friend_nnid = %s AND mii_data IS NOT NULL LIMIT 1",
                    (pnid,),
                )
                row = cur.fetchone()
            if not row:
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
            mii_bytes, fetched_name = await _fetch_pretendo_mii(pnid, session)
            if mii_bytes:
                mii_name = fetched_name or pnid

        if not mii_bytes:
            await interaction.followup.send(
                f"❌ No Mii data found for **{pnid}**.\n"
                f"-# This PNID hasn't been seen by Revivetendo yet. "
                f"Ask them to connect at least once.",
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

    embed = discord.Embed(
        title=mii_name,
        description="[revivetendo.nicochristmann.net](https://revivetendo.nicochristmann.net)",
        color=0x7c3aed,
    )
    embed.set_image(url="attachment://mii.png")
    embed.set_footer(text=f"PNID: {pnid}")

    view = discord.ui.View()
    view.add_item(discord.ui.Button(
        label="Add to Discord",
        url="https://discord.com/oauth2/authorize?client_id=1530990190435369072",
        style=discord.ButtonStyle.link,
        emoji="➕",
    ))
    await interaction.followup.send(embed=embed, file=discord.File(io.BytesIO(img_data), filename="mii.png"), view=view)


@bot.event
async def on_ready():
    print(f"[mii-bot] logged in as {bot.user} (id={bot.user.id})", flush=True)
    try:
        synced = await bot.tree.sync()
        print(f"[mii-bot] global slash commands synced: {[c.name for c in synced]}", flush=True)
    except Exception as e:
        print(f"[mii-bot] global slash command sync FAILED: {e}", flush=True)


asyncio.run(bot.start(TOKEN))
