# Revivetendo Bridge

The backend infrastructure powering **Revivetendo** — a community-run server that bridges the Wii U's original online services with [Pretendo Network](https://pretendo.network), bringing back online functionality for games and apps that Pretendo doesn't officially support yet.

## What does it do?

When a Wii U connects to Revivetendo, it gets routed through our servers which handle authentication, friends, and game-specific online services — all talking to Pretendo's backend where needed. This lets players use features like:

- **WiiU Chat** — video calling between Wii U consoles
- **Miiverse (Juxtaposition)** — posting, communities, and screenshots
- **Mario Kart 8** — online matchmaking and races
- **Wii Sports Club** — online play
- **Angry Birds Star Wars** — online features

## Services

| Directory | Description |
|---|---|
| `account-proxy` | HTTP proxy that handles Wii U account authentication and Mii data fetching |
| `friends-nex` | NEX/PRUDP friends server — presence, friend lists, and notifications |
| `grpc-stubs` | Shared gRPC client stubs for talking to Pretendo's account server |
| `relay-admin` | Web dashboard for monitoring connected users and online status |
| `discord-bot` | Discord bot with PNID linking, WiiU Chat call notifications, and Mii rendering |

## Submodules

| Submodule | Description |
|---|---|
| [`juxt`](https://github.com/Happynico7504/juxtaposition-revivetendo) | Revivetendo fork of Pretendo's Juxtaposition (Miiverse revival) |
| [`wiiu-chat-secure`](https://github.com/PretendoNetwork/wiiu-chat-secure) | Wii U Chat secure server |
| [`wiiu-chat-authentication`](https://github.com/PretendoNetwork/wiiu-chat-authentication) | Wii U Chat authentication server |
| [`mk8-authentication`](https://github.com/Happynico7504/mario-kart-8-authentication) | Mario Kart 8 authentication server |
| [`mk8-secure`](https://github.com/Happynico7504/mario-kart-8-secure) | Mario Kart 8 secure server |
| [`wii-sport-club`](https://github.com/EcrazerDev/wii-sport-club) | Wii Sports Club server |
| [`angry-birds-star-wars`](https://github.com/Happynico7504/angry-birds-star-wars) | Angry Birds Star Wars server |

## Setup

Clone with submodules:

```bash
git clone --recurse-submodules https://github.com/Happynico7504/RevivetendoOpenSource.git
```

Each service reads its configuration from a `.env` file in its directory. See the individual service directories for required variables.

Build and start all services:

```bash
./start.sh
```

## Community

- Website: [revivetendo.nicochristmann.net](https://revivetendo.nicochristmann.net)
- Powered by [Pretendo Network](https://pretendo.network)