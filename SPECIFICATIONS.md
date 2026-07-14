# Twitch Channel Points Miner - Technical Specification

## Table of Contents
1. [Executive Summary](#executive-summary)
2. [System Overview](#system-overview)
3. [Architecture](#architecture)
4. [Core Components](#core-components)
5. [Authentication System](#authentication-system)
6. [Twitch API Integration](#twitch-api-integration)
7. [WebSocket Communication](#websocket-communication)
8. [Point Earning Mechanisms](#point-earning-mechanisms)
9. [Prediction/Betting System](#predictionbetting-system)
10. [Drops & Campaign System](#drops--campaign-system)
11. [Chat Integration](#chat-integration)
12. [Analytics System](#analytics-system)
13. [Configuration System](#configuration-system)
14. [Data Models](#data-models)
15. [Error Handling](#error-handling)

---

## Executive Summary

**Twitch Channel Points Miner** is an automation tool designed to passively earn Twitch channel points by simulating viewer presence across multiple Twitch streams. The application operates headlessly, managing authentication, stream monitoring, automatic bonus claiming, prediction betting, game drops collection, and raid participation without requiring an actual video player or browser.

### Key Capabilities
- **Passive Point Farming**: Earn channel points (+10-12 every 5 minutes) by simulating watch time
- **Automatic Bonus Claiming**: Auto-claim +50 point bonuses when available
- **Watch Streak Detection**: Catch +450 point watch streaks across streamers
- **Raid Following**: Automatically join raids for +250 points
- **Prediction Betting**: Intelligent automated betting on channel predictions
- **Game Drops**: Track and claim game drops from inventory
- **Moments Claiming**: Automatically claim Twitch Moments when available
- **Community Goals**: Contribute channel points to streamer community goals
- **Multi-Streamer Support**: Monitor multiple streamers with priority-based scheduling
- **Real-time Analytics**: Web-based dashboard for tracking point earnings

---

## System Overview

### External Services
| Service | Endpoint | Purpose |
|---------|----------|---------|
| Twitch GQL API | `https://gql.twitch.tv/gql` | GraphQL queries for all Twitch data |
| Twitch PubSub | `wss://pubsub-edge.twitch.tv/v1` | Real-time event notifications |
| Twitch IRC | `irc.chat.twitch.tv:6667` | Chat presence and mentions |
| Twitch OAuth | `https://id.twitch.tv/oauth2/*` | Authentication |
| Twitch CDN | `https://usher.ttvnw.net/*` | Stream playlist URLs |
| Spade Analytics | Dynamic URL from page | Minute-watched reporting |

### Functional Requirements
1. Authenticate with Twitch using OAuth
2. Monitor multiple streamers simultaneously (max 2 active)
3. Simulate watch time to earn channel points
4. Automatically claim available bonuses
5. Participate in predictions with configurable strategies
6. Track and claim game drops
7. Join raids automatically
8. Persist session data between runs
9. Provide analytics on earnings

---

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                                   Miner                                     │
│                          (Main Application Controller)                      │
│                                                                             │
│  ┌───────────────────────────────────────────────────────────────────────┐  │
│  │                         Core Components                               │  │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐   │  │
│  │  │    Auth     │  │   PubSub    │  │    Chat     │  │   Drops     │   │  │
│  │  │   Manager   │  │    Pool     │  │   Manager   │  │   Tracker   │   │  │
│  │  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘   │  │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                    │  │
│  │  │   Watcher   │  │ Predictions │  │Notifications│                    │  │
│  │  │(MinuteWatch)│  │   Handler   │  │   Manager   │                    │  │
│  │  └─────────────┘  └─────────────┘  └─────────────┘                    │  │
│  └───────────────────────────────────────────────────────────────────────┘  │
│                                     │                                       │
│                     ┌───────────────┼───────────────┐                       │
│                     ▼               ▼               ▼                       │
│  ┌─────────────────────┐  ┌─────────────────┐  ┌─────────────────────────┐  │
│  │   Twitch API Client │  │ Analytics       │  │     Web Server          │  │
│  │   (GraphQL)         │  │ Service         │  │     (Dashboard)         │  │
│  │   • GQL Requests    │  │ (Data Layer)    │  │     • Dashboard UI      │  │
│  │   • Stream Info     │  │ • Record Points │  │     • Settings Page     │  │
│  │   • Point Claims    │  │ • Annotations   │  │     • Notifications     │  │
│  └──────────┬──────────┘  │ • Chat Logs     │  │     • Streamer Charts   │  │
│             │             └────────┬────────┘  └───────────┬─────────────┘  │
│             │                      │                       │                │
│             │                      ▼                       │                │
│             │             ┌─────────────────┐              │                │
│             │             │    Database     │◄─────────────┘                │
│             │             │    (SQLite)     │                               │
│             │             └─────────────────┘                               │
└─────────────┼───────────────────────────────────────────────────────────────┘
              │
              ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Twitch Services                                │
│  ┌────────────────┐  ┌────────────────┐  ┌────────────────────────────────┐ │
│  │  GQL API       │  │  PubSub WS     │  │     IRC Chat Server            │ │
│  │  gql.twitch.tv │  │  pubsub-edge   │  │     irc.chat.twitch.tv         │ │
│  └────────────────┘  └────────────────┘  └────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────┘
```

### Module Structure
```
cmd/
└── miner/
    └── main.go                 # Application entry point, signal handling

internal/
├── miner/                      # Main application controller (orchestrator)
│   ├── miner.go                # Coordinates all components, context-based lifecycle
│   └── debug.go                # Assembles the /debug/snapshot document from all components
│
├── streamer/                   # Streamer management
│   └── manager.go              # Loading, storing, updating streamers
│
├── api/                        # Twitch API client
│   └── client.go               # GraphQL requests, stream info, point operations
│
├── auth/                       # Authentication
│   └── auth.go                 # OAuth device flow, token management
│
├── pubsub/                     # WebSocket connections
│   ├── pool.go                 # Connection pool management and message handlers
│   ├── websocket.go            # Individual WebSocket connections
│   ├── message.go              # Message parsing
│   └── topic.go                # Topic types
│
├── chat/                       # IRC chat client
│   ├── manager.go              # Chat connection management
│   └── client.go               # IRC protocol handling
│
├── watcher/                    # Minute-watched tracking
│   ├── watcher.go              # Simulates viewing, reports to Twitch
│   ├── store.go                # Persisted watch-time window (rotation fairness)
│   └── debug.go                # Per-tick selection snapshot for the debug endpoint
│
├── drops/                      # Game drops tracking
│   └── drops.go                # Campaign sync, drop claiming
│
├── discovery/                  # Directory-based channel discovery (extra drops watch slot)
│   └── discovery.go            # Per-game directory sync, candidate pool, auto-switching slot
│
├── debug/                      # Localhost-only diagnostic HTTP server
│   ├── server.go               # 127.0.0.1-bound server: /debug/snapshot, /debug/log
│   └── snapshot.go             # Snapshot JSON document types
│
├── events/                     # In-memory ring buffer of recent miner events
│   └── events.go               # Claims/bets/online-offline history for diagnostics
│
├── analytics/                  # Analytics data layer (no HTTP)
│   ├── service.go              # Point/annotation recording service
│   ├── repository.go           # SQLite data access
│   ├── models.go               # Data models (StreamerData, ChatMessage)
│   └── chat_adapter.go         # Adapter for chat message logging
│
├── web/                        # Web dashboard server
│   ├── server.go               # HTTP server setup, routing, lifecycle
│   ├── responses.go            # HTTP response helpers (writeJSON, writeError)
│   ├── handlers_dashboard.go   # Dashboard and streamer page handlers
│   ├── handlers_analytics.go   # JSON data and chat API handlers
│   ├── handlers_settings.go    # Settings page and API handlers
│   ├── handlers_notifications.go # Notifications page and API handlers
│   ├── handlers_status.go      # Status and health check handlers
│   ├── status.go               # Miner status broadcaster (SSE)
│   ├── viewmodels.go           # Page-specific view models
│   ├── static/                 # CSS, JavaScript assets
│   │   ├── css/app.css
│   │   └── js/
│   └── templates/              # HTML templates
│       ├── base.html
│       ├── dashboard.html
│       ├── streamer.html
│       ├── settings.html
│       ├── notifications.html
│       └── partials/
│
├── notifications/              # Discord notifications
│   ├── manager.go              # Notification orchestration
│   ├── discord.go              # Discord bot client
│   ├── repository.go           # Notification rules storage
│   ├── models.go               # Notification types and config
│   └── provider.go             # Provider interface
│
├── database/                   # Database layer
│   └── database.go             # SQLite connection, migrations
│
├── config/                     # Configuration
│   └── config.go               # Load/save config, defaults
│
├── settings/                   # Runtime settings
│   ├── builder.go              # Settings management for UI
│   ├── convert.go              # Config conversion utilities
│   └── dto.go                  # Data transfer objects
│
├── models/                     # Domain models
│   ├── streamer.go             # Streamer, Stream
│   ├── stream.go               # Stream details, payload
│   ├── prediction.go           # Prediction events
│   ├── bet.go                  # Betting logic and strategies
│   ├── campaign.go             # Drop campaigns
│   ├── drop.go                 # Individual drops
│   ├── community_goal.go       # Community goals
│   ├── raid.go                 # Raid data
│   └── game.go                 # Game info
│
├── constants/                  # Application constants
│   ├── constants.go            # Client IDs, endpoints
│   └── gql.go                  # GraphQL operation definitions
│
├── util/                       # Shared utilities
│   ├── format.go               # Number and time formatting (FormatNumber, FormatDuration, FormatTimeAgo)
│   └── random.go               # Random ID generation (RandomHex, DeviceID)
│
├── logger/                     # Logging
│   └── logger.go               # Structured logging setup
│
└── version/                    # Version info
    └── version.go              # Build version, injected at compile
```

### Package Responsibilities

| Package | Responsibility |
|---------|----------------|
| `miner` | Main application controller. Orchestrates all components, context-based lifecycle. |
| `streamer` | Streamer management. Loading from config, applying settings, session reporting. |
| `api` | Twitch GraphQL API client. All Twitch data fetching and mutations. |
| `auth` | OAuth device flow authentication. Token storage and refresh. |
| `pubsub` | WebSocket connection pool for real-time Twitch PubSub events. |
| `chat` | IRC client for Twitch chat. Presence, mentions, message logging. |
| `watcher` | Minute-watched simulation. Reports viewing activity to Twitch. Context-based cancellation. |
| `drops` | Game drops tracking. Campaign sync and drop claiming. Context-based cancellation. |
| `analytics` | Data layer for points, annotations, chat messages. No HTTP. |
| `web` | HTTP server for dashboard UI. Loopback bind by default; fail-closed startup on non-loopback bind without Basic Auth; same-origin (CSRF) middleware and security headers. See "Dashboard Security Model". |
| `notifications` | Discord bot integration. Mentions, point goals, online/offline alerts. |
| `database` | SQLite database layer. Connection management, migrations. |
| `config` | Configuration loading/saving. Defaults and validation. |
| `settings` | Runtime settings management. UI-driven configuration updates. |
| `models` | Domain models. Streamer, Prediction, Campaign, etc. |
| `util` | Shared utilities. Formatting, random ID generation. |

---

## Core Components

### Orchestrator (Main Controller)

The main controller coordinates all mining operations.

#### Initialization Parameters
| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `username` | string | Required | Twitch username |
| `password` | string | null | Twitch password (prompts if not provided) |
| `claimDropsOnStartup` | boolean | false | Claim all drops from inventory on startup |
| `enableAnalytics` | boolean | true | Enable analytics web server |
| `priority` | array | [STREAK, DROPS, ORDER] | Streamer watching priority |
| `streamerSettings` | object | Default | Default settings for streamers |

#### Core Operations
```
Run(ctx)              # Main entry point, blocks until context is cancelled
initialize()          # Set up connections and load state
authenticate()        # Perform OAuth login
loadStreamers()       # Load streamers via StreamerManager
startMining(ctx)      # Begin the mining loop with context
stop()                # Graceful shutdown
```

#### Lifecycle Model

The application uses `context.Context` for lifecycle management:
- Signal handling (SIGINT, SIGTERM) is done in `main.go` using `signal.NotifyContext`
- The context is passed to `Miner.Run(ctx)` which propagates it to all components
- When the context is cancelled, all goroutines gracefully shut down

#### Concurrent Operations
The application runs multiple concurrent operations, all using context-based cancellation:
1. **Minute Watcher**: Sends minute-watched events (60s cycle divided by # of streamers, with ±20% jitter)
2. **Campaign Sync**: Syncs drop campaigns every 60 minutes
3. **Stream Check Loop**: Periodic online status checks
4. **WebSocket Handlers**: One per PubSub connection (up to 50 topics each)
5. **IRC Connections**: One per streamer with chat enabled
6. **Analytics Server**: HTTP server for dashboard (optional)

---

## Authentication System

### OAuth Device Flow

The application uses the TV device OAuth flow for authentication.

#### Authentication Sequence
```
1. POST /oauth2/device
   Request: { client_id, scopes }
   Response: { device_code, user_code, verification_uri, expires_in, interval }

2. Display to user:
   - URL: https://www.twitch.tv/activate
   - Code: {user_code}

3. Poll /oauth2/token every {interval} seconds
   Request: { client_id, device_code, grant_type: "device_code" }
   Response: { access_token, refresh_token, token_type }

4. Store access_token for future use
```

#### Token Storage
- Tokens persisted locally between sessions at `cookies/{username}.json` (mode `0600`)
- Contains: `auth_token`, `user_id`, `username`
- Only the access token is persisted (there is no refresh flow)

##### Encryption at Rest

The stored token can be encrypted with AES-256-GCM. Encryption is controlled by
the `TWITCH_AUTH_ENCRYPTION_KEY` environment variable (an env var, never a
config-file field — `config.json` is itself plaintext):

- **Unset** → the record is written in the legacy plaintext JSON layout
  (`{auth_token, user_id, username}`), and a one-time warning is logged.
- **Set** → the inner record is AES-256-GCM sealed and stored as a versioned
  envelope; the key is derived with PBKDF2-HMAC-SHA256 (600k iterations):

  ```json
  { "version": 2, "kdf": "pbkdf2-sha256", "iter": 600000,
    "salt": "<b64>", "nonce": "<b64>", "ciphertext": "<b64>" }
  ```

Format detection is by the `version` field (absent = legacy plaintext). Salt and
nonce are random per save; the derived key is zeroed after use; the passphrase and
token are never logged.

**Migration / failure modes:**
- Plaintext file + passphrase now set → migrated to the encrypted envelope in
  place on load (`SaveAuth` re-write), **no re-login**.
- Encrypted file + missing/changed passphrase, or a tampered ciphertext →
  `LoadStoredAuth` returns an error and `Login()` falls back to the device flow.
  This is the only condition that forces re-authentication.
- Encryption touches only `SaveAuth`/`LoadStoredAuth`; the in-memory token
  (`GetAuthToken`) and all consumers (API/PubSub/IRC) are unchanged.

#### Required Request Headers
```
Authorization: OAuth {access_token}
Client-Id: ue6666qo983tsx6so1t0vnawi233wa
Client-Session-Id: {random_hex_16_chars}
Client-Version: {twilight_build_id}
User-Agent: {tv_user_agent}
X-Device-Id: {random_32_char_string}
```

#### Client Identifiers
| Type | Value | Use Case |
|------|-------|----------|
| TV Client | `ue6666qo983tsx6so1t0vnawi233wa` | Recommended |
| Browser | `kimne78kx3ncx6brgo4mv6wki5h1ko` | Alternative |
| Mobile | `r8s4dac0uhzifbpu9sjdiwzctle17ff` | Alternative |

---

## Twitch API Integration

### GraphQL Operations

All Twitch API interactions use persisted GraphQL queries with SHA256 hashes.

#### Operation Format
```json
{
    "operationName": "OperationName",
    "variables": { ... },
    "extensions": {
        "persistedQuery": {
            "version": 1,
            "sha256Hash": "..."
        }
    }
}
```

#### Available Operations

| Operation | SHA256 Hash | Purpose |
|-----------|-------------|---------|
| `WithIsStreamLiveQuery` | `04e46329a6786ff3a81c01c50bfa5d725902507a0deb83b0edbf7abe7a3716ea` | Check if stream is live |
| `PlaybackAccessToken` | `3093517e37e4f4cb48906155bcd894150aef92617939236d2508f3375ab732ce` | Get stream playback token |
| `VideoPlayerStreamInfoOverlayChannel` | `a5f2e34d626a9f4f5c0204f910bab2194948a9502089be558bb6e779a9e1b3d2` | Get stream info |
| `ClaimCommunityPoints` | `46aaeebe02c99afdf4fc97c7c0cba964124bf6b0af229395f1f6d1feed05b3d0` | Claim bonus points |
| `CommunityMomentCallout_Claim` | `e2d67415aead910f7f9ceb45a77b750a1e1d9622c936d832328a0689e054db62` | Claim moments |
| `DropsPage_ClaimDropRewards` | `a455deea71bdc9015b78eb49f4acfbce8baa7ccbedd28e549bb025bd0f751930` | Claim drops |
| `ChannelPointsContext` | `1530a003a7d374b0380b79db0be0534f30ff46e61cffa2bc0e2468a909fbc024` | Get channel points |
| `JoinRaid` | `c6a332a86d1087fbbb1a8623aa01bd1313d2386e7c63be60fdb2d1901f01a4ae` | Join a raid |
| `Inventory` | `d86775d0ef16a63a33ad52e80eaff963b2d5b72fada7c991504a57496e1d8e4b` | Get user inventory |
| `MakePrediction` | `b44682ecc88358817009f20e69d75081b1e58825bb40aa53d5dbadcc17c881d8` | Place prediction bet |
| `ViewerDropsDashboard` | `5a4da2ab3d5b47c9f9ce864e727b2cb346af1e3ea8b897fe8f704a97ff017619` | Get drop campaigns |
| `DropCampaignDetails` | `f6396f5ffdde867a8f6f6da18286e4baf02e5b98d14689a69b5af320a4c7b7b8` | Get campaign details |
| `DropsHighlightService_AvailableDrops` | `9a62a09bce5b53e26e64a671e530bc599cb6aab1e5ba3cbd5d85966d3940716f` | Get available drops |
| `GetIDFromLogin` | `94e82a7b1e3c21e186daa73ee2afc4b8f23bade1fbbff6fe8ac133f50a2f58ca` | Get user ID from username |
| `ChannelFollows` | `eecf815273d3d949e5cf0085cc5084cd8a1b5b7b6f7990cf43cb0beadf546907` | Get followed channels |
| `ContributeCommunityPointsCommunityGoal` | `5774f0ea5d89587d73021a2e03c3c44777d903840c608754a1be519f51e37bb6` | Contribute to goals |
| `RedeemCustomReward` | `d56249a7adb4978898ea3412e196688d4ac3cea1c0c2dfd65561d229ea5dcc42` | Redeem custom channel-points reward (renamed server-side from `RedeemCommunityPointsCustomReward`) |
| `DirectoryPage_Game` | `cb5dc816e139dcb8a118f14b4b677d59abc224a4b016c4bc2bb00a47fe0ddec4` | List live channels in a game directory (drops-only via `options.systemFilters: ["DROPS_ENABLED"]`); hash rotates every few months — track DevilXD/TwitchDropsMiner's constants.py |
| `DirectoryGameRedirect` | `1f0300090caceec51f33c5e20647aceff9017f740f223c3c532ba6fa59f6b6cc` | Resolve a game display name to its directory slug (`game(name:) { id slug }`) |

---

## WebSocket Communication

### PubSub Protocol

#### Connection
- Endpoint: `wss://pubsub-edge.twitch.tv/v1`
- Max topics per connection: 50
- Max connections per IP: 10 (recommended)

#### Message Types

**Outgoing:**
```json
// Listen to topic
{
    "type": "LISTEN",
    "nonce": "{random_30_char_string}",
    "data": {
        "topics": ["topic-name.channel_id"],
        "auth_token": "{oauth_token}"  // For user topics
    }
}

// Heartbeat
{ "type": "PING" }
```

**Incoming:**
```json
// Topic message
{
    "type": "MESSAGE",
    "data": {
        "topic": "topic-name.channel_id",
        "message": "{json_string}"
    }
}

// Heartbeat response
{ "type": "PONG" }

// Reconnection required
{ "type": "RECONNECT" }

// Error
{ "type": "RESPONSE", "error": "ERR_BADAUTH" }
```

### Topic Types

| Topic | Format | Auth Required | Purpose |
|-------|--------|---------------|---------|
| `community-points-user-v1` | `.{user_id}` | Yes | Points earned/spent |
| `predictions-user-v1` | `.{user_id}` | Yes | Prediction confirmations |
| `video-playback-by-id` | `.{channel_id}` | No | Stream status |
| `raid` | `.{channel_id}` | No | Raid events |
| `predictions-channel-v1` | `.{channel_id}` | No | New predictions |
| `community-moments-channel-v1` | `.{channel_id}` | No | Moments available |
| `community-points-channel-v1` | `.{channel_id}` | No | Community goals |

### Event Handlers

| Topic | Message Type | Action |
|-------|--------------|--------|
| `community-points-user-v1` | `points-earned` | Update balance, log earnings |
| `community-points-user-v1` | `points-spent` | Update balance |
| `community-points-user-v1` | `claim-available` | Auto-claim bonus |
| `video-playback-by-id` | `stream-up` | Mark streamer online |
| `video-playback-by-id` | `stream-down` | Mark streamer offline |
| `video-playback-by-id` | `viewcount` | Verify streamer status |
| `raid` | `raid_update_v2` | Join raid |
| `community-moments-channel-v1` | `active` | Claim moment |
| `predictions-channel-v1` | `event-created` | Schedule prediction bet |
| `predictions-channel-v1` | `event-updated` | Update prediction outcomes |
| `predictions-user-v1` | `prediction-result` | Log bet result |
| `predictions-user-v1` | `prediction-made` | Confirm bet placed |
| `community-points-channel-v1` | `community-goal-*` | Update/contribute to goals |

### Connection Management
- Send PING at configured interval (default 27s) with ±2.5s random jitter
- Reconnect if no PONG received within 5 minutes
- Auto-reconnect on disconnect with 60-second delay
- Check internet connectivity before reconnecting

---

## Point Earning Mechanisms

### Earning Methods

| Method | Points | Trigger |
|--------|--------|---------|
| Watch Time | +10-12 | Every 5 minutes of watching |
| Bonus Claim | +50 | Click bonus button (auto-claimed) |
| Watch Streak | +300-450 | Returning for consecutive streams |
| Raid Participation | +250 | Joining a raid |
| Predictions (Win) | Variable | Winning a prediction bet |

### Minute-Watched System

To earn watch time points, the application must report viewing activity.

#### Request Flow
```
1. Get Playback Token
   POST gql.twitch.tv/gql (PlaybackAccessToken)
   Variables: { login, isLive: true, isVod: false, playerType: "site" }
   Returns: { signature, value }

2. Get Stream Playlist
   GET usher.ttvnw.net/api/channel/hls/{channel}.m3u8
   Params: { sig, token, player_type, allow_source: true }
   Returns: M3U8 playlist with quality options

3. Parse Playlist
   Extract lowest quality stream URL (160p preferred)

4. Request Stream Segment
   GET {lowest_quality_url}
   This validates active viewing

5. Report Minute Watched
   POST {spade_url}
   Content-Type: application/x-www-form-urlencoded
   Body: data=<url-encoded base64(json_payload)>
```

The base64 payload is placed in a form field (`data=...`) and **must be
percent-encoded** — standard base64 can contain `+`, which a form parser would
otherwise decode as a space and corrupt the event (mirrors the reference python
miner's `requests` form post and the web player's `btoa` + `encodeURIComponent`).

#### Spade URL Discovery
```
1. GET https://www.twitch.tv/{channel}
2. Parse HTML for settings URL: /config/settings.*.js
3. GET settings URL
4. Parse for "spade_url": "{url}"
```

#### Minute-Watched Payload
```json
[{
    "event": "minute-watched",
    "properties": {
        "channel_id": "123456",
        "broadcast_id": "789012",
        "player": "site",
        "user_id": "456789",
        "live": true,
        "channel": "streamer_name",
        "game": "Game Name",      // Optional: for drops
        "game_id": "12345"        // Optional: for drops
    }
}]
```

### Watch Slot Architecture

**All configured and discovered channels compete for the same maximum of two
Twitch watch slots. Directory Discovery never creates an independent third
watch session.**

The `MinuteWatcher` (`internal/watcher`) is the **unified slot broker**: the
single owner of the (at most `constants.MaxSimultaneousStreams` = 2) Twitch
watch slots and the only component that drives `MinuteSender`. Every source
of a watchable channel only *proposes candidates*; the broker alone decides
who occupies a slot and does the minute-watched reporting.

```
Configured streamers ─┐
Discovery candidates ─┤
Drop candidates ──────┼── Unified Slot Broker ── Slot 1
Streak candidates ────┤                         └─ Slot 2
Fair rotation ────────┘
```

Each tick the broker runs two phases:

- **Phase A — configured selection** (unchanged): the priority/rotation logic
  below picks up to two channels from the configured streamer list (direct
  priority pick when ≤2 online, fair rotation with a DROPS/STREAK boost when
  more).
- **Phase B — cross-source arbitration**: candidate sources (directory
  discovery today) are layered on top. A candidate fills any free slot;
  otherwise it may displace the lowest-ranked configured occupant it strictly
  out-ranks — except one within minutes of completing a watch streak, which
  is never interrupted. A channel already holding a slot never gets a second
  one. Ranking (high→low): channel-restricted drop → in-progress watch streak
  → active drop → fair-rotation/priority pick. With no candidate sources
  Phase B is a pure pass-through, so single-list behavior is unchanged.

The broker publishes an immutable, explainable snapshot each tick
(`BrokerSnapshot`: per-slot `channel`/`source`/`reasonCode`/`reason`/
`campaign`, plus a `waiting` list) consumed by the Overview "Сейчас смотрим"
block, the Drops/discovery page, and `/debug/snapshot`. Slot changes (a
channel taking/leaving a slot, or its reason changing) are logged at INFO and
recorded as `slot_assigned`/`slot_released` events; a steady state logs
nothing, so the same decision is not repeated every minute.

Concurrency: `priorities`/`settings` are loop-owned and read lock-free during
selection; `UpdateSettings` stages a change under a mutex that the loop
applies at the start of the next tick (runtime settings without restart, no
data race). The published snapshot is swapped via an atomic pointer, so the
dashboard, the debug endpoint, and discovery read it without taking any broker
lock, and no lock is ever held across a Twitch GQL call, a spade beacon, or a
SQLite write.

The same staging pattern carries the drop-progress watchdog's session repair:
`RequestSessionRefresh(login, mode)` stages a request under the mutex, and the
loop executes it at the start of its next tick — only for a channel that still
holds a slot — publishing the outcome atomically (`LastSessionRefresh`).
Refreshes for distinct channels run in parallel (worker goroutines joined
before the sends), so the tick-delay bound is the per-channel maximum (up to 4
network rounds × the api client's 30s timeout for a full session recreate),
never the sum across slots; the budget math against the minute-watched
continuity window (`maxContinuousGap = 2×interval`) and the benign consequence
of exceeding it in the pathological worst case (a streak-continuity reset that
mirrors Twitch's own server-side session break) are documented on
`executeSessionRefreshes`. Each worker mutates only its own slotted streamer
and joins before any send, so the broker loop remains the sole effective
writer of live watch sessions; no external goroutine ever mutates a slotted
streamer. The loop
also publishes per-slot minute-watched delivery accounting (`ReportStats`) each
tick, and consults an optional avoid checker during selection (a temporarily
avoided channel is skipped exactly like `DisableWatch`, but the exclusion
expires on its own).

**The one documented exception:** the watch-transport health canary (see *Health
Signals*) may send a single real minute-watched beacon to a dedicated channel to
verify the transport, opportunistically when a broker slot is free or once the
transport has not been confirmed for a configurable max-staleness window. It
never holds a broker slot and is not a candidate source; at most one extra beacon
can briefly coincide with two busy slots, and only on the max-staleness schedule.

### Priority System

Maximum 2 streams watched simultaneously (`constants.MaxSimultaneousStreams`),
allocated by the unified slot broker (see *Watch Slot Architecture*).

**2 or fewer online streamers:** all of them are watched; the priority list below picks which ones fill the (at most 2) watch slots, same as always:

| Priority | Behavior |
|----------|----------|
| `STREAK` | Prioritize streamers with pending watch streak (< 7 min watched, > 30 min since offline) |
| `DROPS` | Prioritize streamers with active drop campaigns |
| `SUBSCRIBED` | Prioritize subscribed channels (higher tiers first) |
| `ORDER` | Follow order in streamers list |
| `POINTS_ASCENDING` | Lowest points first |
| `POINTS_DESCENDING` | Highest points first |

**More than 2 online streamers:** a fixed priority pick would starve every other online channel indefinitely, so the watched pair instead rotates fairly across all online streamers. See `internal/watcher.selectRotating` (and `store.go` for persistence) for the full algorithm:

- **Randomized dwell time:** every time the pair actually changes, the next dwell duration is drawn uniformly from `[rateLimits.rotationIntervalMinMinutes, rateLimits.rotationIntervalMaxMinutes]` (default 30-80 min) rather than using one fixed timer, so rotations don't happen on a single predictable period. (`rateLimits.rotationInterval`, a fixed-seconds field, is deprecated - kept only so pre-existing config.json files still parse; `LoadConfig` migrates it into the new min/max fields the first time it loads such a file.)
- **Weighted base pair:** when the dwell time elapses (or a pair member goes offline), the pair is recomputed from each online streamer's accumulated watch minutes over the trailing 8-hour window - persisted in SQLite (`watch_time_events` table, module `watch_time`, survives container restarts) - and the two with the *least* accumulated time get the slots. Ties (e.g. cold start, nobody watched yet) are broken by in-memory recency, then index, for determinism. This is a deficit-based scheduler: whoever gets watched accumulates minutes and becomes less eligible next time, which surfaces every other online channel over time regardless of count or parity - no even/odd special-casing needed.
- **Priority as a boost, not exclusivity:** on top of the weighted base pair, any online streamer with an active drop (`DROPS`) or an in-progress watch streak (`STREAK`) can take over one seat in the pair for the current tick only, without affecting the weighting above - increasing how often it's picked, never granting a permanent exclusive slot. The seat sacrificed is whichever base-pair member was watched most recently. Among competing boost candidates the seat goes, in order: to a channel-restricted drop campaign (its progress is only countable on that exact channel), then to a **watch streak already in progress, most-watched-first** (so the watcher finishes a streak it already started instead of alternating between several fresh pending-streak channels and completing none - a watch streak needs ~5-7 continuous watched minutes, so an interrupted pursuit earns nothing), then least-recently-watched.
- **Continuous-watch accounting:** `Stream.MinuteWatched` measures *continuous* watched minutes, not wall-clock elapsed time. Each successful minute-watched report credits the gap since the previous report only if that gap is within `2 × minuteWatchedInterval`; a larger gap means the streamer lost its watch slot (rotated out, a failed cycle, a brief offline blip) and, since Twitch resets its server-side watch-streak session on such a break, the local counter is **reset to zero** and the gap is credited as nothing. Without this, `MinuteWatched` would accumulate wall-clock time across rotation gaps, cross the streak threshold on phantom minutes the viewer never continuously watched, and - because the pursuit logic stops chasing a streamer once it passes the threshold - abandon a streak that was in fact never earned.
- **Watch-streak pursuit diagnostics:** because a watch streak is only ever logged once *earned* (a `points-earned` PubSub event with `reason_code = WATCH_STREAK`, `~+300-450`), a streak that is never earned is otherwise invisible. The watcher therefore logs, per streak, one INFO when it starts pursuing a streamer's pending streak and one WARN if the streamer has been watched past the streak threshold (`watchStreakThresholdMinutes`, 7) *continuously* while the streak is still missing - the WARN distinguishes "not watched enough yet" (a scheduling problem) from "watched enough but Twitch never credited it" (an upstream/authorization/viewing problem).
- **Avoiding last-second interruptions:** a scheduled swap-out is postponed once, by a short fixed delay (2 min), if the leaving streamer is within a few minutes of completing its watch streak - but only when both current pair members are still online (an offline member is dropped immediately, regardless of streak state). This doesn't extend to imminent drop-campaign completion.
- Predictions/bets are unaffected by this rotation: PubSub subscribes to prediction topics for every tracked online streamer regardless of its current watch-pair membership, so bets are placed independently of what's actively being watched.

---

## Prediction/Betting System

### Betting Strategies

| Strategy | Logic |
|----------|-------|
| `MOST_VOTED` | Choose option with most users |
| `HIGH_ODDS` | Choose option with highest odds |
| `PERCENTAGE` | Choose option with highest win percentage |
| `SMART_MONEY` | Choose option with highest top bet |
| `SMART` | If user gap > `percentageGap`: follow majority; else: choose highest odds |
| `NUMBER_1` through `NUMBER_8` | Always choose specific outcome position |

### Bet Settings

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `strategy` | enum | SMART | Betting strategy to use |
| `percentage` | int | 5 | Percentage of balance to bet |
| `percentageGap` | int | 20 | Gap threshold for SMART strategy |
| `maxPoints` | int | 50000 | Maximum points per bet |
| `minimumPoints` | int | 0 | Minimum balance required to bet |
| `stealthMode` | bool | false | Bet slightly less than top bettor |
| `delayMode` | enum | FROM_END | When to place bet |
| `delay` | float | 6 | Delay value (meaning depends on mode) |
| `filterCondition` | object | null | Conditions to skip betting |

### Filter Conditions

Bets can be filtered based on:

| Key | Description | Aggregation |
|-----|-------------|-------------|
| `PERCENTAGE_USERS` | User percentage on decision | Per outcome |
| `ODDS_PERCENTAGE` | Win percentage based on odds | Per outcome |
| `ODDS` | Raw odds value | Per outcome |
| `TOP_POINTS` | Highest bet amount | Per outcome |
| `DECISION_USERS` | Users on chosen outcome | Per outcome |
| `DECISION_POINTS` | Points on chosen outcome | Per outcome |
| `TOTAL_USERS` | Total users betting | Sum |
| `TOTAL_POINTS` | Total points in pool | Sum |

**Operators**: `GT`, `LT`, `GTE`, `LTE`

**Example**: Skip if total users < 200
```json
{
    "by": "TOTAL_USERS",
    "where": "GTE",
    "value": 200
}
```

### Delay Modes

| Mode | Behavior |
|------|----------|
| `FROM_START` | Wait `delay` seconds after bet opens |
| `FROM_END` | Wait until `delay` seconds before bet closes |
| `PERCENTAGE` | Wait until `delay`% of timer elapsed |

### Prediction Lifecycle

```
1. event-created (PubSub)
   ├── Status: ACTIVE
   ├── Parse outcomes, timer
   └── Schedule bet placement

2. event-updated (PubSub, multiple times)
   ├── Update outcome stats (users, points)
   └── Calculate odds, percentages

3. Bet Placement (timed)
   ├── Apply strategy
   ├── Check filters
   ├── Calculate amount
   └── POST MakePrediction

4. prediction-made (PubSub)
   └── Confirm bet recorded

5. prediction-result (PubSub)
   ├── Status: WIN/LOSE/REFUND
   └── Update statistics
```

---

## Drops & Campaign System

### Campaign Structure

```
Campaign
├── id: string
├── name: string
├── game: { id, displayName }
├── status: ACTIVE | EXPIRED
├── startAt: datetime
├── endAt: datetime
├── allowedChannels: string[] (empty = all)
├── drops: Drop[]
├── claimStatus: in_progress | already_claimed
└── claimedDropNames: string[] (rewards stripped by claim-history check)
```

### Drop Structure

```
Drop
├── id: string
├── name: string
├── benefit: string (reward description)
├── requiredMinutesWatched: int
├── currentMinutesWatched: int
├── percentageProgress: int
├── hasPreconditionsMet: bool
├── dropInstanceId: string (null until started)
├── isClaimable: bool
├── isClaimed: bool
├── startAt: datetime
└── endAt: datetime
```

### Drop Claiming Flow

```
1. Sync Campaigns (every 60 minutes)
   ├── GET ViewerDropsDashboard (status: ACTIVE)
   ├── GET DropCampaignDetails for each
   ├── Backfill campaign date window (startAt/endAt) from the dashboard
   │   summary when the details response omits it, then recompute the
   │   date-window match, so an active campaign isn't dropped as "outside its
   │   date window" while a genuinely-expired details window is still honored
   └── Filter by date range

   (One concise INFO line is logged per sync — dashboard count, recovered-
   from-inventory count, and tracked count — and the same figures plus the
   tracked campaign list are exposed at GET /debug/snapshot under "drops", so
   an empty Drops page is diagnosable without -debug.)

2. Sync Inventory
   ├── GET Inventory
   ├── Match drops to campaigns
   ├── Update progress
   └── Recover any dropCampaignsInProgress campaign missing from the
       dashboard/details path (build it straight from the inventory entry, no
       date-window gating), so a campaign Twitch is actively crediting always
       appears on the Drops page even when its DropCampaignDetails fetch
       returned nothing

2b. Apply Claim History
   ├── GET Inventory (gameEventDrops: account-wide granted rewards)
   ├── Normalize each granted reward to a game+name key
   ├── Strip any campaign drop whose normalized key was already granted
   │   (covers recurring/regional variants of the same campaign under a
   │   different campaign or drop ID)
   ├── Campaign.claimStatus = already_claimed once all its drops are stripped
   └── Log "already claimed" (campaign) and skipped-drop cases

3. Check Claimable
   ├── dropInstanceId != null
   ├── isClaimed == false
   └── currentMinutesWatched >= requiredMinutesWatched

4. Claim Drop
   ├── POST DropsPage_ClaimDropRewards
   └── Mark as claimed
```

### Lightweight progress sync

The full sync above runs only every `campaignSyncInterval` minutes (60 by
default) because it is expensive: a `ViewerDropsDashboard` listing plus one
`DropCampaignDetails` fetch per active campaign plus several `Inventory`
reads. On its own that leaves the dashboard/Drops-page progress up to a full
interval stale — a campaign shown at 58% (140/240 min) while Twitch already
credits ~69%.

To keep the displayed progress within a minute or two of Twitch's real
progress, `DropsTracker` runs a second, much cheaper loop
(`progressLoop`/`syncProgress`) on `dropProgressSyncInterval` minutes (2 by
default, range 1-60):

```
Progress sync (dropProgressSyncInterval, or on demand)
   ├── GET Inventory   (single query; no dashboard listing, no per-campaign
   │                    DropCampaignDetails fetches)
   ├── For each already-tracked campaign the inventory reports progress for:
   │   clone it, advance its drops' currentMinutesWatched from the inventory
   │   `self` data, drop claimed/out-of-window drops
   └── Republish the campaign pool (fresh objects, swapped under lock so the
       Drops page and directory discovery keep reading immutable published
       campaigns) only when progress actually changed
```

It never discovers, claims, or filters campaigns — those stay with the full
sync — it only advances the watched-minute counters of campaigns the full
sync already published (a new campaign still appears at the next full sync).
The watcher calls `DropsTracker.TriggerProgressSync` after every successfully
reported watched minute, so a watched minute is reflected on the Drops page
within seconds rather than waiting out the interval.

### Drops Eligibility

A streamer is eligible for drops when:
- `claimDrops` setting is enabled
- Streamer is online
- Stream has active campaign IDs
- Campaign game matches stream game

### Claim History Check

Before a campaign can make a streamer eligible for the `PriorityDrops`
channel-selection boost, `DropsTracker.applyClaimHistory`
(`internal/drops/drops.go`) cross-references each of its drops against the
account's Twitch-wide claim history (`gameEventDrops` in the `Inventory`
response) via `Drop.RewardKey` / `Campaign.ApplyClaimHistory`
(`internal/models/drop.go`, `internal/models/campaign.go`).

Reward identity is normalized as `lower(gameID) + "::" + lower(dropName)`
rather than trusting Twitch's raw campaign/drop IDs, since a recurring or
regional variant of the same campaign reuses the same reward name and game
under a different (and occasionally colliding) campaign/drop ID. A drop
whose normalized key already appears in the claim history is stripped from
`Campaign.Drops` before campaigns are matched to streamers; if that empties
a campaign, its `ClaimStatus` becomes `already_claimed`. Since
`updateStreamerCampaigns` only assigns campaigns with `len(Drops) > 0`, an
already-claimed campaign is never used to prioritize channel selection or
consume watch time. Each skip is logged (`slog.Info`, "already claimed" /
"already-claimed reward") naming the campaign and which rewards were already
granted. `ClaimStatus`/`ClaimedDropNames` are kept on the (still in-memory)
campaign list rather than discarded, so a future dashboard view can list
already-claimed campaigns separately from in-progress ones without further
backend changes.

### Channel-Restricted Campaigns

A campaign's `allowedChannels` (parsed from GraphQL `allow.channels`) is either
empty (any channel streaming the game credits progress) or a specific list of
channel IDs (only those channels credit progress).

Per-channel eligibility is determined authoritatively by Twitch: each
configured streamer's `CampaignIDs` comes from a per-channel query
(`DropsHighlightServiceAvailableDrops`, scoped by `channelID`), so a channel
that isn't in a campaign's allowed list won't have that campaign's ID
returned for it in the first place. `updateStreamerCampaigns`
(`internal/drops/drops.go`) additionally cross-checks `allowedChannels`
against the streamer's own channel ID as a defensive second layer, logging a
warning and withholding the campaign if it ever mismatches (see
`Campaign.AllowsChannel`).

Because a channel-restricted campaign can only ever progress by watching that
exact channel, the watcher's `DROPS` priority and rotation boost
(`internal/watcher/watcher.go`) treat streamers holding one as higher
priority than streamers whose active campaigns are all unrestricted — an
unrestricted campaign's progress could in principle also be earned by
watching a different configured streamer with the same game, so it's safer
to spend a limited watch slot on the channel-restricted one first. The
dashboard shows a "Channel-only drop" badge on a streamer's card when this
applies.

### Directory-Based Channel Discovery (`internal/discovery`)

An optional subsystem (config key `directoryGames`, a list of game names;
empty = disabled) that farms drops for games *without* requiring any matching
channel in the configured streamer list. It is a **candidate source for the
unified slot broker** (see *Watch Slot Architecture* below), not an
independent watch slot: it proposes channels and the broker decides whether
they occupy one of the two Twitch watch slots, competing on equal footing
with the configured streamer list. Discovered channels are ephemeral
`models.Streamer` objects that never enter the streamer manager, PubSub pool,
chat, rotation fairness store, or drops-claiming path of the configured list.

Flow, per configured game:

1. **Eligibility** — a game is only queried while the drops tracker holds at
   least one active, unclaimed campaign for it (matched by game name against
   `DropsTracker.Campaigns()`, which is already filtered by date window,
   claim history, and the drop-name blacklist). When the final reward of a
   game's last campaign is claimed, the game drops out of discovery
   automatically.
2. **Directory sync** — `DirectoryPage_Game` (slug resolved via
   `DirectoryGameRedirect` with per-game caching and a local slugify
   fallback; a slug that stops resolving is evicted and re-resolved) lists
   up to 30 live channels with `systemFilters: ["DROPS_ENABLED"]`, sorted by
   viewer count. Channels already on the configured streamer list are
   excluded — they belong to the rotation, and double-watching one channel
   would duplicate its minute-watched reporting. The sync runs every
   `campaignSyncInterval` minutes, dropping to a 2-minute retry while the
   pool is empty (or when every candidate has been verified unwatchable). A
   failed query keeps the game's previous candidates.
3. **Proposing a candidate** — the best candidate (configured game order,
   then viewers descending, mirroring reference miners' top-by-viewers
   pick) is verified online via the normal `CheckStreamerOnline` path (spade
   URL + stream payload + per-channel campaign IDs) and **proposed to the
   slot broker** through `WatchCandidates()`; discovery never sends
   minute-watched itself. The broker places the proposal in a slot (and does
   the actual `MinuteSender` reporting) only when a slot is free or the
   proposal out-prioritizes a configured occupant — see *Watch Slot
   Architecture*. Candidate preparation runs on the broker's loop goroutine,
   so a discovered channel's `models.Streamer` is only ever touched by that
   one goroutine plus locked `State()` reads. Channel eligibility requires an
   intersection between the channel's available campaign IDs and the
   tracker's active unclaimed campaigns (honoring channel-restricted
   allow-lists) — the same check `updateStreamerCampaigns` performs for
   tracked streamers — so a channel carrying only an already-claimed
   recurring campaign is never farmed. At most 3 candidates are
   online-verified per tick to bound API bursts.
4. **Auto-switching** — the slot abandons its channel and moves to the next
   candidate when the channel goes offline, switches game, no longer
   carries a tracker-active campaign (claimed/blacklisted ones don't
   count), the game's campaigns are exhausted, or the channel/game is
   removed from (or the channel is added to) the relevant settings lists.
   Log lines: `Discovered channel selected`, `Switching discovered
   channel`, and `Discovery pool empty` (once per transition).

Drop progress earned this way lands in the account inventory and is claimed
by the existing drops tracker (`claimAllDropsFromInventory` / inventory
sync) — discovery itself never claims.

No PubSub topics are subscribed for discovered channels: online state is
maintained by directory syncs plus the stale-stream re-check, so the
subsystem adds zero WebSocket connections. All of its GQL calls go through
the shared client and therefore inherit the retry/backoff, the
PersistedQueryNotFound client-ID fallback, and the connection-health
watchdog's `LastSuccessAt` accounting.

Twitch only credits watch time for up to 2 simultaneous streams
(`constants.MaxSimultaneousStreams`). Discovery therefore competes for one of
those two slots rather than adding a third: when both slots are already held
by configured streamers (and no discovered channel out-prioritizes them),
discovery's proposal simply waits, shown as `available` on the Drops page.
Discovery is most effective when fewer than two configured streamers are live
— e.g. overnight — where it fills the otherwise-idle slots. A discovered
channel is reported as `watching` only when the broker actually placed it in a
slot; its per-channel watch-minute accounting is visible on the Drops page.

---

## Health Signals (`internal/health`)

The Health Center aggregates the miner's operational signals for the dashboard
(`/health`) and the debug snapshot (`/debug/snapshot`, `health` section). Each
signal records only `status` (`ok`/`failed`/`idle`/`unknown`), `checkedAt`,
`duration`, `stage`, a short human `detail`, and a stable `errorCode` —
**never** an OAuth token, cookies, a signed playback/spade URL (which embeds
`sig`/`token`), or an authorization header.

The signals are distinct kinds of health:

- **OAuth** — whether the account authorization is still valid (from the miner's
  reauth-required state).
- **GQL API** — whether Twitch GraphQL calls are succeeding (from the API
  client's last-success timestamp vs `connectionTimeoutMinutes`).
- **PubSub** — whether the WebSocket pool has recent activity (last PONG/message).
- **Watch Transport** — whether Twitch *accepts the watch transport and beacon*
  (from the canary, below). This is independent of whether any drop is active.
- **Drops Inventory Sync** — whether the periodic inventory sync is running
  without error (from the drops tracker's sync status).
- **Drops Progress** — composed by the drop-progress watchdog (below): `ok`
  while every tracked drop advances (with a `recovering:<stage>` marker while
  the pipeline runs), `stalled` once a drop's stall is confirmed and automatic
  recovery is exhausted, `idle` when nothing is tracked.

The **Active GQL Client ID** (TV / Browser / Mobile) is also shown, since the
client can promote a fallback ID after a `PersistedQueryNotFound`.

### Watch-transport accrual canary

The canary verifies the watch transport end-to-end by reusing the production
beacon path — there is no second beacon implementation. It exposes
`MinuteSender.Probe`, which runs the same steps as `MinuteSender.Send` (playback
access token → HLS playlist → lowest-quality variant → segment HEAD → spade
`minute-watched` POST, `application/x-www-form-urlencoded` with the base64 body
percent-encoded), stage-instrumented and redacted.

- **Scheduling (hybrid).** When enabled, the canary confirms the transport on a
  target `canaryIntervalMinutes` cadence **opportunistically** — only when a
  broker watch slot is free — and is **forced** (regardless of slot occupancy)
  once the transport has not been confirmed for `canaryMaxStalenessHours`. It is
  the single documented, rare exception to the two-slot rule (see *Watch Slot
  Architecture*): never a permanent slot, never a candidate source.
- **On demand.** "Run canary now" triggers an immediate probe; concurrent runs
  are suppressed (an atomic in-flight guard) and each probe has a 60s timeout and
  honors context cancellation end-to-end. The prober itself is context-aware
  (`http.NewRequestWithContext`), but the two Twitch client calls a probe needs
  first — `GetChannelID` and `CheckStreamerOnline` — are not, so the canary runs
  them under a watchdog (`runDetached`): on timeout/cancel the probe returns at
  once and the still-running call is *abandoned*, not killed. **Known limitation
  (bounded leak):** an abandoned goroutine self-terminates once the api client's
  own HTTP timeout (30s per attempt × retries × client-id candidates) elapses, so
  the leak is temporary, never permanent — threading a context through the whole
  GQL stack would touch every Twitch call in the app, so the fix is confined to
  the canary. A timed-out `CheckStreamerOnline` also drops the cached probe
  streamer so the abandoned writer never shares it with a later probe (no data
  race). This is documented like the cold-start victim-by-index tie-break and the
  `consoleWriter` check-then-send micro-window.
- **Honest limitation.** The canary confirms Twitch accepts the watch transport
  and beacon requests; **without an active drop campaign it does not prove
  accrual of a specific drop.** The UI and this document state this explicitly.
- **Notifications.** Transitions (healthy→failed, failed→recovered) reuse the
  system-notification channel (Discord + Matrix/Pushover/Gotify/webhook). Only
  transitions notify — repeated same-state results never spam.

Config (`health`, all runtime-updatable from the Health Center without a
restart): `canaryEnabled` (default `false`), `canaryChannel` (empty disables it),
`canaryIntervalMinutes` (default 360, clamped [60, 1440]), `canaryMaxStalenessHours`
(default 48, clamped [1, 168], and additionally floored to the interval so the
forced-probe threshold always covers at least one opportunistic cycle — otherwise
the force condition fires first and the hybrid degenerates into "always force").

### Drop progress watchdog

The watchdog (`internal/health/progress.go`) detects the failure no connection
watchdog can see: everything upstream healthy, yet a specific drop's
`currentMinutesWatched` stops advancing. It keeps one state per tracked
campaign's current drop (`campaignID+dropID`): last observed minutes, when they
last advanced, delivered watch reports since then, consecutive clean
no-progress observations, status (`healthy`/`recovering`/`stalled`), and the
recovery stage reached.

**Stall confirmation is conjunctive** — every gate must hold simultaneously,
and any failing gate is named in the published state (explainability). All
three thresholds (delay, observations, delivered reports) count only inside
the current **evidence window**: it opens when every gate starts holding and
is discarded whenever any gate fails, so a confirmed stall always represents
at least `watchdogStallDelayMinutes` of *demonstrable* farming without credit.
Evidence accrued while the channel was offline, rotated out, or ineligible
never carries over — otherwise a stall would confirm minutes after farming
resumes, inside Twitch's ~15-minute crediting batch. A gate failure pauses
the recovery pipeline (the reached stage survives) but each next stage
requires a fresh, complete evidence window:

1. campaign `ACTIVE`, not past `endAt`; drop inside its date window;
2. drop not claimable and not claimed (claimable = fully progressed — the
   claim flow's job, not a stall);
3. a slotted channel is farming the campaign (`IsWatching` + the tracker's
   campaign↔channel intersection still assigns it);
4. the channel has not switched games (`Stream.GameID()` vs campaign game);
5. `HasPreconditionsMet` is not explicitly false;
6. minute-watched reports are demonstrably delivered — the broker's new
   per-slot delivery accounting shows ≥5 successes since the last progress;
7. ≥ `watchdogStallConfirmations` consecutive inventory observations completed
   **successfully** without progress ("checked and unchanged", never "could not
   check" — the tracker's progress sync now records
   `ProgressLastSyncAt`/`ProgressLastError`, errored reads never count, one
   observation is never counted twice, and the observation cursor is seeded on
   episode start so the read whose data *showed* the last progress — or one
   completed before tracking began — can never count);
8. more than `watchdogStallDelayMinutes` of evidence-window time;
9. inventory currently observable: the last progress-sync attempt did not
   error, and a successful observation completed within the stall-delay window
   (an invisible Twitch-side credit during an inventory outage must not be
   declared a stall);
10. no Twitch outage evidence (OAuth/GQL/PubSub/watch-transport signals not
    FAILED in the health center).

**Recovery pipeline** — finite, ordered, at most one stage execution per
evaluation pass (≈1 min, jittered), each stage cooldown-bounded
(`watchdogRecoveryCooldownMinutes`), idempotent, ctx-bounded (60s), and visible
in `/debug/snapshot` (`progressWatchdog` section) and the events feed:

1. forced lightweight inventory sync (`TriggerProgressSync`);
2. forced full campaign resync (`SyncNow` — dashboard, details, inventory, and
   the campaign/channel intersection recompute; serialized against the
   background loop, run via `runDetached`);
3. stream-info refresh — **staged into the slot broker**
   (`RequestSessionRefresh(login, stream_info)`): the broker loop executes it
   at its next tick for the slot it owns (forced past the 2-minute
   `UpdateRequired` gate) and publishes the outcome;
4. watch-transport probe (`MinuteSender.Probe`, ctx-aware, stage-instrumented).
   The sender caches no playback token or playlist — both are fetched fresh on
   every send — so the spec's "refresh token/playlist" steps are honestly
   implemented as a verified fresh fetch with the failing stage reported;
5. watch-session recreate — staged into the broker
   (`RequestSessionRefresh(login, session)`): spade URL re-scrape + forced
   stream-info/payload rebuild, the online-streamer equivalent of the
   offline→online bring-up;
6. channel switch via the **avoid list**: the watchdog never commands the
   broker — it marks the channel avoided for `watchdogAvoidTTLMinutes`, and the
   broker/discovery stop selecting it, so arbitration picks the next eligible
   channel while the broker keeps sole slot authority;
7. one critical operator notification (system channel), transition-only. The
   episode is then terminal (`stalled`) until progress resumes (full reset +
   recovered notification + avoid entry cleared) or `watchdogRearmHours`
   elapses (silent pipeline re-arm, no duplicate alert).

**Concurrency architecture.** The watchdog goroutine never mutates a live
streamer: mutating recovery stages are staged into the broker loop (the same
single-writer staging pattern as `UpdateSettings`), the probe stage is
read-only, and `Stream`'s spade URL / campaign fields moved behind locked
accessors so the api client, drops tracker, broker, and watchdog no longer race
on them. There is deliberately no imperative "switch channel" API.

Config (`health`, runtime-updatable): `watchdogEnabled` (default **true** — the
deliberate opt-out asymmetry with the opt-in canary: detection is passive reads
of existing state, costs no extra Twitch calls, and recovery only follows a
conservatively confirmed stall), `watchdogStallDelayMinutes` (20, clamped
[10, 120] — Twitch credits minutes in ~15-minute batches), 
`watchdogStallConfirmations` (3, clamped [2, 10]), `watchdogRecoveryCooldownMinutes`
(5, clamped [1, 60]), `watchdogAvoidTTLMinutes` (60, clamped [10, 360]),
`watchdogRearmHours` (6, clamped [1, 48]).

Known limitation: if after a channel switch no eligible channel picks the
campaign up, the state stays `recovering` with the explanatory "no slotted
channel is farming" detail — the terminal notification only fires once a
channel demonstrably farms without progress, because notifying on "nobody is
farming" would alert on ordinary rotation/offline gaps.

---

## Campaign Policy Engine

`internal/policy` is a pure, deterministic, side-effect-free ranker (no I/O, no
globals, no `time.Now()` — the caller passes `now` and pre-assembled
`CampaignInput` snapshots). It never allocates a watch slot; it only orders
candidates and produces an explainable decision per campaign. No opaque model.

### Feasibility (estimate, not a guarantee)

Per campaign: `timeUntilEnd`, `minutesToNextReward` (the lowest-threshold
unmet drop's remaining), `minutesToCompleteAll` (the furthest milestone's
remaining — the codebase's cumulative model), `canCompleteNextReward`,
`canCompleteAll` (both against `timeUntilEnd − safetyReserve`), and a status:
`SAFE` (finishes the chain with margin), `AT_RISK` (finishes but the margin is
thin), `NEXT_REWARD_ONLY` (only the next reward is reachable), `IMPOSSIBLE`
(not even the next reward, or already ended). The `NextRewardOnly` rule reduces
the goal to just the next reward.

### Modes

`GAME_ORDER` (default, orders by configured game index — bit-identical to the
pre-engine behavior), `ENDING_SOONEST`, `CLOSEST_TO_REWARD`, `LOW_AVAILABILITY`
(fewest eligible live channels first), and `SMART`. `Normalize` upper-cases and
falls back to `GAME_ORDER`; `ValidateConfig` canonicalizes the persisted value
via the same validator (single source of the valid-mode set).

### SMART scoring (itemized)

A weighted sum of named factors, each rendered as a breakdown line: high
priority (+200), channel-restricted (+100), ends within 6h (+80), reward
closeness (tiered +60/+40/+20), sole eligible channel (+30), already-started
(+40), already-in-a-slot stickiness (+10), unstable channel (up to −50), and a
−40 penalty when only the next reward is reachable. Ranking ties break on
campaign ID, so identical inputs always produce identical output.

**Channel-stability sample gate.** The instability penalty is derived from the
Stage 3 per-slot delivery accounting (`watcher.ReportStats`: successes/failures
of minute-watched sends), but only participates once the sample count reaches a
minimum; below it the factor is neutral (0 points) and labeled *insufficient
data* — the same cold-start guard as the Stage 1 displacement tie-break, so a
one- or two-observation window never masquerades as a confident 0%/100% signal.

### Per-drop controls

Keyed by `models.NormalizeRewardKey` (lowercased `gameID::dropName`), not a
transient Twitch drop ID, so a rule survives recurring/regional variants that
grant the identical reward. Flags: `Skip` (exclude), `HighPriority` (float to
top in every mode), `AlwaysFinishStarted`, `NextRewardOnly`,
`IgnoreSubscriberOnly` (a no-op — surfaced honestly in the UI — unless Twitch
reports the subscriber-only flag, which it does not reliably expose on
time-based drops). Stored in the top-level `dropRules` config map (like
`autoRedeem`) so it round-trips through Settings untouched; the zero value
resets the rule.

### Integration (broker keeps slot authority)

On the health-watchdog tick (no new goroutine, no Twitch calls — inputs come
from already-synced state) the miner assembles inputs, ranks them, and
publishes: per-login best scores to the watcher (`SetCampaignScores`, read
lock-free) and per-game ranks to discovery (`SetGameRanks`). The watcher's
`DROPS` priority orders competing drop streamers by score **within** its
existing restricted-first passes — the channel-restricted-first invariant is
never overridden — and discovery builds its cross-game pool in policy order.
With no scores/ranks published (GAME_ORDER default, no rules) both paths are
bit-identical to before. Config (`campaignPolicy`, `dropRules`) is
runtime-updatable via the Drops page; the ranked decisions surface on the Drops
page (feasibility badge + breakdown + per-drop controls) and in
`/debug/snapshot` (`policy` section — mode, scores, factors; no secrets).

---

## Chat Integration

### IRC Protocol

| Setting | Value |
|---------|-------|
| Server | `irc.chat.twitch.tv` |
| Port | `6667` (plain) or `6697` (SSL) |
| Auth | `PASS oauth:{token}` |

#### Connection Sequence
```
1. Connect to server
2. CAP REQ :twitch.tv/tags twitch.tv/commands  (if chat logging enabled)
3. PASS oauth:{token}
4. NICK {username}
5. JOIN #{channel}
```

#### IRC Capabilities

| Capability | Purpose |
|------------|---------|
| `twitch.tv/tags` | Receive message metadata (emotes, badges, color) |
| `twitch.tv/commands` | Receive Twitch-specific IRC messages |

These capabilities are only requested when chat logging is enabled to reduce bandwidth.

### Chat Presence Modes

| Mode | Behavior |
|------|----------|
| `ALWAYS` | Always connected to IRC |
| `NEVER` | Never connect to IRC |
| `ONLINE` | Connect when streamer is online |
| `OFFLINE` | Connect when streamer is offline |

### Chat Logging

When enabled (`analytics.enableChatLogs: true`), chat messages are stored in SQLite with:
- Username and display name
- Message content
- Emote positions (Twitch format: `emote_id:start-end/...`)
- Badge list
- User color

Messages can be searched via the dashboard or API endpoint.

### Features
- Appears in viewer list
- May earn StreamElements points
- Detects @mentions (logs to console)
- Optional chat message logging with emote support

---

## Database System

### Unified Database

All application data is stored in a single SQLite database (`database/{username}/miner.db`). The database uses a modular migration system that tracks schema versions per module, allowing different parts of the application to manage their own migrations independently.

#### Schema Versioning

Schema versions are tracked per-module in the `schema_versions` table:

```sql
CREATE TABLE schema_versions (
    module TEXT PRIMARY KEY,
    version INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
```

This design allows:
- **Independent module migrations**: Each module (analytics, notifications, etc.) can add migrations without affecting others
- **Future-proof extensibility**: New modules can be added without modifying existing migration code
- **Clear version tracking**: Easy to see which version each module is at

#### Analytics Module Schema

```sql
-- Streamers table
CREATE TABLE streamers (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT UNIQUE NOT NULL,
    created_at INTEGER NOT NULL
);

-- Points history
CREATE TABLE points (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer_id INTEGER NOT NULL,
    timestamp INTEGER NOT NULL,
    points INTEGER NOT NULL,
    event_type TEXT,
    FOREIGN KEY (streamer_id) REFERENCES streamers(id)
);

-- Annotations (predictions, streaks, etc.)
CREATE TABLE annotations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer_id INTEGER NOT NULL,
    timestamp INTEGER NOT NULL,
    text TEXT NOT NULL,
    color TEXT NOT NULL,
    FOREIGN KEY (streamer_id) REFERENCES streamers(id)
);

-- Chat messages (optional, when enableChatLogs is true)
CREATE TABLE chat_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer_id INTEGER NOT NULL,
    timestamp INTEGER NOT NULL,
    username TEXT NOT NULL,
    display_name TEXT NOT NULL,
    message TEXT NOT NULL,
    emotes TEXT,
    badges TEXT,
    color TEXT,
    FOREIGN KEY (streamer_id) REFERENCES streamers(id)
);

-- Prediction bets (migration v4) — one row per resolved prediction, powering
-- ROI analytics. UNIQUE(event_id) makes recording idempotent against a
-- re-delivered prediction-result (PubSub reconnect). No FOREIGN KEY: this
-- codebase never enables PRAGMA foreign_keys, so an FK would be decorative;
-- streamer_id integrity is instead guaranteed by resolving/creating the parent
-- streamer row before insert (as every table here already does).
CREATE TABLE prediction_bets (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer_id  INTEGER NOT NULL,
    event_id     TEXT NOT NULL UNIQUE,
    timestamp    INTEGER NOT NULL,
    strategy     TEXT NOT NULL,       -- SMART/HIGH_ODDS/…/MANUAL
    result_type  TEXT NOT NULL,       -- WIN | LOSE | REFUND
    placed       INTEGER NOT NULL,    -- raw stake (kept even for REFUND)
    won          INTEGER NOT NULL,    -- payout (0 for LOSE/REFUND)
    gained       INTEGER NOT NULL,    -- net (won-placed for WIN/LOSE, 0 for REFUND)
    odds         REAL NOT NULL,       -- chosen outcome's odds at resolution
    manual       INTEGER NOT NULL DEFAULT 0
);

-- Indexes for performance
CREATE INDEX idx_points_streamer_time ON points(streamer_id, timestamp);
CREATE INDEX idx_annotations_streamer_time ON annotations(streamer_id, timestamp);
CREATE INDEX idx_chat_streamer_time ON chat_messages(streamer_id, timestamp);
CREATE INDEX idx_predbets_streamer_time ON prediction_bets(streamer_id, timestamp);
```

`prediction_bets` is deliberately **excluded** from the retention sweep
(`PruneBefore` only prunes `points` and `annotations`), so lifetime ROI stays
exact; it grows by one row per resolved prediction. Migration v4 is additive
(no `ALTER` of existing tables), so it is safe to apply to a populated database.

#### Notifications Module Schema

```sql
-- Notification configuration (single row)
CREATE TABLE notification_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    mentions_channel_id TEXT DEFAULT '',
    points_channel_id TEXT DEFAULT '',
    online_channel_id TEXT DEFAULT '',
    offline_channel_id TEXT DEFAULT '',
    mentions_enabled INTEGER DEFAULT 0,
    mentions_all_chats INTEGER DEFAULT 1,
    mentions_streamers TEXT DEFAULT '[]',
    online_enabled INTEGER DEFAULT 0,
    online_all_streamers INTEGER DEFAULT 1,
    online_streamers TEXT DEFAULT '[]',
    offline_enabled INTEGER DEFAULT 0,
    offline_all_streamers INTEGER DEFAULT 1,
    offline_streamers TEXT DEFAULT '[]'
);

-- Point notification rules
CREATE TABLE point_rules (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer TEXT NOT NULL,
    threshold INTEGER NOT NULL,
    delete_on_trigger INTEGER DEFAULT 0,
    triggered INTEGER DEFAULT 0
);
```

#### Watch-Time Rotation Module Schema

```sql
-- Per-streamer watch-time credits, used to rank who's most "owed" a turn in
-- the fair watch-pair rotation (see Priority System above). Timestamps are
-- Unix seconds (unlike the analytics/notifications tables above).
CREATE TABLE watch_time_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer TEXT NOT NULL,
    timestamp INTEGER NOT NULL,
    minutes REAL NOT NULL
);

CREATE INDEX idx_watch_time_streamer_time ON watch_time_events(streamer, timestamp);
```

Rows older than 2x the 8-hour ranking window are opportunistically pruned on write, keeping the table bounded over long uptimes. This data persists across restarts (same `/database` volume, same modular migration system as the other modules above).

#### Drop Catalog Module Schema

```sql
-- Durable catalog of every drop campaign the miner has observed (current +
-- upcoming), so the Drops-page "Past" tab can show campaigns that have expired
-- and dropped off Twitch's dashboard (which only returns active + upcoming).
-- One row per campaign INSTANCE (campaign_id is unique); a recurring campaign
-- accumulates one row per occurrence, grouped in the UI by campaign_key
-- (NormalizeRewardKey(gameID, campaignName)).
CREATE TABLE drop_campaigns (
    campaign_id   TEXT PRIMARY KEY,
    campaign_key  TEXT NOT NULL,   -- game + campaign name, for recurring grouping
    name          TEXT NOT NULL,
    game          TEXT,
    start_at      INTEGER NOT NULL DEFAULT 0,  -- Unix millis (0 = unknown)
    end_at        INTEGER NOT NULL DEFAULT 0,
    status        TEXT,
    claimed       INTEGER NOT NULL DEFAULT 0,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL
);

CREATE INDEX idx_drop_campaigns_key_end ON drop_campaigns(campaign_key, end_at);
```

Upsert semantics (`ON CONFLICT(campaign_id) DO UPDATE`): `last_seen_at`, `status`,
`claimed`, `name`, and `game` refresh on each observation; `start_at`/`end_at`
refresh only when the new observation actually carries a date (a `CASE … > 0`
guard, so a later date-less Twitch response can't zero out good dates); and
`first_seen_at` is never in the SET list, so it keeps the first-observed moment.
The catalog is **excluded from the retention sweep** (`PruneBefore`) — its whole
point is long memory, and it grows only one row per campaign instance. Upcoming
campaigns are captured display-only from the same `ViewerDropsDashboard` response
already fetched each sync (no new Twitch calls) and are **never** entered into the
active farm set — a campaign cannot be farmed before its official start.

**Note**: All timestamps are Unix timestamps in milliseconds, except `watch_time_events.timestamp` which is Unix seconds.

---

## Analytics System

The analytics system is split into two packages:
- **`internal/analytics`**: Data layer for recording and querying points, annotations, and chat messages (no HTTP)
- **`internal/web`**: HTTP server providing the dashboard UI, settings, and notifications pages

### Dashboard Authentication

The web dashboard supports optional HTTP Basic Authentication via environment variables:

| Variable | Description |
|----------|-------------|
| `DASHBOARD_USERNAME` | Username for dashboard access |
| `DASHBOARD_PASSWORD` | Password for dashboard access |

Both must be set to enable authentication. When enabled, all dashboard routes require valid credentials.

### Data Storage

Analytics data is stored in the unified database (`database/{username}/miner.db`) under the analytics module.

### Prediction ROI Analytics

Resolved prediction bets are persisted to `prediction_bets` and aggregated into a
read-only ROI report on the Statistics page. The data flow avoids touching the
betting engine:

1. **Emit** — When a confirmed prediction resolves, `pubsub.WebSocketPool`
   (`handlePredictionUser`, the same place that already updates streamer history)
   builds a `pubsub.BetResult` and hands it to the `SetBetResultHandler` sink.
   The raw stake is read from `event.Bet.Decision.Amount` **before**
   `ParseResult` (which zeroes `placed` on a REFUND), the strategy from
   `event.Bet.Settings.Strategy` (or `"MANUAL"` for a dashboard bet), and the
   odds from the chosen outcome. The handler is invoked outside the pool lock.
2. **Persist** — The miner maps `BetResult` to `analytics.BetRecord` and calls
   `Service.RecordBet`, which does an idempotent `INSERT OR IGNORE`
   (UNIQUE(event_id)); a re-delivered result is logged, not double-counted.
3. **Aggregate** — `analytics.ComputeROI([]BetRecord) ROISummary` is a pure,
   deterministic function (no I/O, no `time.Now`): the caller supplies the
   period-filtered records, it computes counts, win rate, wagered, net profit,
   ROI, averages, maximum drawdown, and the by-streamer/by-strategy/by-odds
   breakdowns. Buckets: `<1.5 / 1.5–2 / 2–3 / 3–5 / 5+` (upper bound exclusive).
4. **Serve** — `GET /api/predictions/roi?streamer=&strategy=&period=` returns the
   summary; `GET /api/predictions/roi/export` returns the raw bets as a JSON
   attachment. Periods: `7d / 30d / 90d / lifetime` (lifetime = open-ended).

Metric conventions: win rate, average wager, and total wagered are over settled
bets (WIN + LOSE); refunds return the stake and are counted separately. Net
profit is the sum of `gained`; ROI = net profit ÷ total wagered × 100. Maximum
drawdown is the largest peak-to-trough drop of the cumulative net-profit curve.
The report never places, modifies, or auto-disables a bet or strategy.

### Event Types for Series

| Event | Description |
|-------|-------------|
| `Watch` | Points from watching |
| `Claim` | Points from bonus claim |
| `Watch Streak` | Watch streak bonus |
| `Raid` | Raid participation |
| `Prediction` | Prediction result |
| `Spent` | Points spent |

### Annotation Types

| Type | Color | Description |
|------|-------|-------------|
| `WATCH_STREAK` | Blue (#45c1ff) | Watch streak earned |
| `PREDICTION_MADE` | Yellow (#ffe045) | Bet placed |
| `WIN` | Green (#36b535) | Prediction won |
| `LOSE` | Red (#ff4545) | Prediction lost |

### Web Dashboard HTTP Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Dashboard HTML page |
| `/streamer/{name}` | GET | Streamer detail page with chart and chat |
| `/settings` | GET | Runtime settings page |
| `/notifications` | GET | Discord notifications management page |
| `/streamers` | GET | List of streamers with current points |
| `/json/{streamer}` | GET | JSON data for specific streamer |
| `/json_all` | GET | All streamers' data combined |
| `/api/streamers` | GET | Streamer grid partial (HTMX) |
| `/api/chat/{streamer}` | GET | Chat messages JSON |
| `/api/status` | GET | Connection status |
| `/api/miner-status` | GET | Current miner status JSON |
| `/api/miner-status/stream` | GET | SSE stream for miner status updates |
| `/api/settings` | GET/POST | Get or update runtime settings |
| `/api/settings/reset` | POST | Reset settings to defaults |
| `/api/followed` | GET | List the authenticated user's followed channels for the import picker (each flagged `alreadyTracked`; `truncated`/`cap` report the pagination limit) |
| `/api/followed/import` | POST | Add selected followed channels (`{"logins":[...]}`) to the tracked streamer list with default settings; returns `added` count |

#### Query Parameters for `/json/{streamer}`
- `startDate`: Filter start (YYYY-MM-DD)
- `endDate`: Filter end (YYYY-MM-DD)

#### Query Parameters for `/api/chat/{streamer}`
- `limit`: Max messages to return (default: 50, max: 200)
- `offset`: Pagination offset
- `q`: Search query (searches message, username, display name)

#### Followed-Channel Import

The Settings page can seed the tracked streamer list from the account's Twitch
follows. `GET /api/followed` calls the `ChannelFollows` persisted query through
the miner's existing token (no extra OAuth scope), paginating on
`edges[].cursor` / `pageInfo.hasNextPage`:

- The paginator (`api.collectFollowedChannels`, network injected as a `fetch`
  closure so it is unit-testable) dedups logins case-insensitively and stops at
  `maxFollowedFetch = 1000` (`followedPageSize = 100` per request). Hitting the
  cap while Twitch still reports more pages returns `truncated=true`, which the
  UI surfaces as "showing first 1000 of more" rather than silently cutting.
- The handler marks each channel `alreadyTracked` against the current streamer
  list and sorts untracked-first-then-alphabetical so the actionable rows lead.
- `POST /api/followed/import` appends the selected logins via the miner's
  standard `ApplySettings` path — **default settings, no per-streamer
  overrides** — skipping any already tracked, then resolves channel IDs,
  subscribes PubSub topics, and persists `config.json`. It returns the number of
  **newly** added entries. This is a one-shot import, not a background sync.

---

## Configuration System

### Streamer Settings

Per-streamer configuration options:

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `makePredictions` | bool | true | Enable betting |
| `followRaid` | bool | true | Join raids |
| `claimDrops` | bool | true | Claim game drops |
| `claimMoments` | bool | true | Claim moments |
| `watchStreak` | bool | true | Prioritize watch streaks |
| `communityGoals` | bool | false | Contribute to goals |
| `communityGoalsMaxPercent` | int | 10 | Cap per contribution to this % of current balance (0 = no limit; used only when `communityGoals` is true) |
| `communityGoalsMaxAmount` | int | 0 | Absolute point cap per contribution (0 = no limit; the lower of this and the % cap wins) |
| `chat` | enum | ONLINE | IRC presence mode |
| `chatLogs` | bool* | null | Override global chat logging (null = use global) |
| `bet` | object | Default | Betting configuration |

### Settings Priority
1. Per-streamer settings specified individually
2. Default streamer settings from configuration
3. Built-in defaults

### Logger Settings

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `save` | bool | true | Save logs to file |
| `less` | bool | false | Reduced verbosity mode |
| `consoleLevel` | enum | INFO | Console log level |
| `fileLevel` | enum | DEBUG | File log level |
| `emoji` | bool | true | Enable emoji in logs |
| `colored` | bool | false | Enable colored output |
| `autoClear` | bool | true | Log rotation (7 days) |
| `timeZone` | string | null | Custom timezone |

### Rate Limit Settings

Defaults are tuned to match the Python miner and avoid Twitch rate limiting. Random jitter is applied to intervals to appear more human-like.

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `websocketPingInterval` | int | 27 | Base seconds between WebSocket pings (20-60), ±2.5s jitter applied |
| `campaignSyncInterval` | int | 60 | Minutes between full drop campaign syncs — discovery, claiming, filtering (5-120) |
| `dropProgressSyncInterval` | int | 2 | Minutes between lightweight inventory-only drop-progress refreshes shown on the Drops page; also triggered right after each watched minute (1-60) |
| `minuteWatchedInterval` | int | 60 | Base seconds for minute-watched cycle (30-120), divided by # of streamers, ±20% jitter |
| `requestDelay` | float | 0.5 | Seconds between consecutive API calls (0.1-2.0) |
| `reconnectDelay` | int | 60 | Seconds to wait before reconnecting (30-300) |
| `streamCheckInterval` | int | 600 | Seconds between stream status checks (60-900) |
| `rotationIntervalMinMinutes` | int | 30 | Minimum minutes the watch pair dwells before rotating, when > 2 tracked streamers are online (5-180) |
| `rotationIntervalMaxMinutes` | int | 80 | Maximum minutes the watch pair dwells before rotating; a random value in [Min, Max] is drawn on every rotation (5-240) |
| `rotationInterval` | int | - | Deprecated fixed-seconds fallback, migrated into the two fields above on load; not read anywhere else |

---

## Data Models

### Streamer

```
Streamer
├── username: string
├── channelId: string
├── settings: StreamerSettings
├── isOnline: bool
├── onlineAt: timestamp
├── offlineAt: timestamp
├── channelPoints: int
├── communityGoals: map<string, CommunityGoal>
├── activeMultipliers: array (subscription bonuses)
├── stream: Stream
├── raid: Raid
└── history: map<string, { counter: int, amount: int }>
```

### Stream

```
Stream
├── broadcastId: string
├── title: string
├── game: { id, name }
├── tags: string[]
├── viewersCount: int
├── spadeUrl: string
├── payload: array (minute-watched data)
├── campaignIds: string[]
├── campaigns: Campaign[]
├── minuteWatched: int
└── watchStreakMissing: bool
```

### Prediction/EventPrediction

```
EventPrediction
├── streamerId: string
├── eventId: string
├── title: string
├── createdAt: datetime
├── predictionWindowSeconds: int
├── status: ACTIVE | LOCKED | RESOLVED | CANCELED
├── outcomes: Outcome[]
├── bet: Bet
├── betPlaced: bool
├── betConfirmed: bool
└── result: { type: WIN|LOSE|REFUND, gained: int }
```

### Outcome

```
Outcome
├── id: string
├── title: string
├── color: string (BLUE, PINK, etc.)
├── totalUsers: int
├── totalPoints: int
├── topPoints: int (highest individual bet)
├── percentageUsers: float
├── odds: float
└── oddsPercentage: float
```

### Campaign

```
Campaign
├── id: string
├── name: string
├── game: { id, displayName }
├── status: string
├── startAt: datetime
├── endAt: datetime
├── channels: string[] (allowed channel IDs)
├── inInventory: bool
└── drops: Drop[]
```

### Drop

```
Drop
├── id: string
├── name: string
├── benefit: string
├── minutesRequired: int
├── currentMinutesWatched: int
├── percentageProgress: int
├── hasPreconditionsMet: bool
├── dropInstanceId: string
├── isClaimable: bool
├── isClaimed: bool
├── startAt: datetime
└── endAt: datetime
```

### CommunityGoal

```
CommunityGoal
├── goalId: string
├── title: string
├── description: string
├── status: STARTED | ENDED
├── pointsContributed: int
├── goalAmount: int
├── perStreamUserMaxContribution: int
└── isInStock: bool
```

**Contribution mechanics.** The `ContributeCommunityPointsCommunityGoal` mutation
accepts an arbitrary integer `amount` in its input, so contributions can be any
partial value — the API is not restricted to fixed steps or an all-in amount.
The only server-imposed ceiling per stream is `perStreamUserMaxContribution`.
The miner therefore contributes `min(amountLeft, balance, perStreamUserMax,
maxPercent%·balance, maxAmount)` where the last two terms are the user-configured
limits (`communityGoalsMaxPercent` / `communityGoalsMaxAmount`, `0` disabling
each). Every contribution is logged with the amount spent and the remaining
balance so total spend is auditable.

### Raid

```
Raid
├── raidId: string
└── targetLogin: string
```

---

## Error Handling

### Error Types

| Error | Description | Recovery |
|-------|-------------|----------|
| `StreamerDoesNotExist` | Invalid streamer username | Skip streamer |
| `StreamerIsOffline` | Streamer not currently live | Mark offline, retry later |
| `BadCredentials` | Authentication failed | Re-authenticate |
| `InvalidCookies` | Corrupted session data | Delete and re-authenticate |
| `ERR_BADAUTH` | WebSocket auth failed | Delete cookies, re-authenticate |
| `ConnectionLost` | Network disconnection | Reconnect with backoff |

### Reconnection Strategy

**WebSocket:**
1. Detect disconnect (no PONG, connection error)
2. Set reconnecting flag
3. Wait 60 seconds
4. Check internet connectivity
5. Create new connection
6. Re-subscribe to all topics

**HTTP Requests:**
1. Catch connection error
2. Check internet connectivity
3. Wait 1-3 minutes (random)
4. Retry request

### Graceful Shutdown

On termination signal:
1. Stop all IRC connections
2. Close WebSocket pool
3. Wait for background operations to complete
4. Save any pending state
5. Print final session report

---

## File Structure

```
application/
├── config.json               # User configuration
├── cookies/
│   └── {username}.pkl        # Authentication tokens (pickle format)
├── logs/
│   └── {username}.log        # Log files (7-day rotation)
└── database/
    └── {username}/
        └── miner.db          # Unified SQLite database (analytics, notifications, etc.)
```

---

## Rate Limits & Constraints

### Fixed Limits (Twitch-Imposed)

| Constraint | Value | Notes |
|------------|-------|-------|
| Max simultaneous streams | 2 | Twitch limitation, cannot be changed |
| WebSocket topics per connection | 50 | API limit |
| WebSocket connections per IP | 10 | Recommended limit |

### Configurable Limits

Defaults are tuned to match the Python miner. Random jitter is applied to avoid detection.

| Setting | Default | Min | Max | Description |
|---------|---------|-----|-----|-------------|
| `websocketPingInterval` | 27 | 20 | 60 | Base seconds between WebSocket pings (±2.5s jitter) |
| `campaignSyncInterval` | 60 | 5 | 120 | Minutes between full drop campaign syncs (discovery, claiming, filtering) |
| `dropProgressSyncInterval` | 2 | 1 | 60 | Minutes between lightweight inventory-only drop-progress refreshes (also on each watched minute) |
| `minuteWatchedInterval` | 60 | 30 | 120 | Base seconds for minute-watched cycle (divided by # streamers, ±20% jitter) |
| `requestDelay` | 0.5 | 0.1 | 2.0 | Seconds between consecutive API calls |
| `reconnectDelay` | 60 | 30 | 300 | Seconds to wait before reconnecting |
| `streamCheckInterval` | 600 | 60 | 900 | Seconds between stream status checks |
| `rotationIntervalMinMinutes` | 30 | 5 | 180 | Minutes the watch pair dwells before rotating (minimum of the random range) |
| `rotationIntervalMaxMinutes` | 80 | 5 | 240 | Minutes the watch pair dwells before rotating (maximum of the random range; clamped up to Min if set lower) |

---

## Daily Summary

An optional once-a-day operator digest for the previous full local day, sent via
the notification system channel (`NotifyDailySummary`, `NotificationType =
daily_summary`). Config block `dailySummary { enabled bool, time "HH:MM" }` —
opt-in (off by default); the time is canonicalized in `ValidateConfig` (invalid →
09:00). Scheduling is a dedicated `dailySummaryLoop` in the miner: it arms a
`time.Timer` to the next local `HH:MM`, recomputes on each fire (so it survives
DST), is idempotent per date, and exits on context cancellation. It never fires
for a partial day on startup.

Metric sources — durable (SQLite) vs best-effort (in-memory, reset on restart):

| Metric | Source | Durable? |
|--------|--------|----------|
| Net points (earned) | `EarnedPointsBetween` — sum over streamers of (last − first) balance in the window | yes |
| Prediction net | `GetBets` → `ComputeROI().NetProfit` (Prediction ROI engine) | yes |
| Watch streaks | `CountAnnotationsByType("WATCH_STREAK", …)` | yes |
| Drops claimed | `CountAnnotationsByType("DROP_CLAIMED", …)` — recorded on each claim under the hidden `(drops)` analytics bucket, which `ListStreamers` excludes | yes |
| Recovery incidents | count of drop-watchdog events in the in-memory event ring buffer within the window | best-effort |
| Lost mining time | watcher accumulator: per tick `max(0, min(MaxSlots, fillable) − watchedOK) × interval` — slots that were fillable (a live candidate existed) but produced no watched minute; drained on send | best-effort |

The rendered message presents the prediction net as a **component** of net points
(e.g. `Net points: +910 (of which +390 from predictions)`), never as a parallel
number, and labels the best-effort figures as such. Earned points is a global
sum across all streamers; net delta already includes betting outcomes, which is
why the prediction line is a component of it rather than additive.

**Known limitation:** `dailySummary.enabled`/`time` are read once at startup, not
hot-reloaded like the runtime Settings-page fields. Changing them requires a
restart. Because the field is never reassigned after start, the loop reads it
lock-free with no data race (other config fields that *are* mutated at runtime
live at different struct offsets and do not race with these reads).

## Notification System

The miner supports Discord notifications for various events. The notification system is designed with a provider interface allowing future extension to other notification services (Telegram, Slack, etc.).

### Discord Integration

Discord notifications require a Discord bot. Configuration is stored in the config file (connection settings only), while notification rules are stored in the SQLite database.

#### Configuration

| Setting | Type | Description |
|---------|------|-------------|
| `discord.enabled` | bool | Enable/disable Discord notifications (requires restart) |
| `discord.botToken` | string | Discord bot token |
| `discord.guildId` | string | Discord server (guild) ID |

#### Notification Types

| Type | Description | Configuration |
|------|-------------|---------------|
| **Chat Mentions** | Notifies when someone mentions you in chat | Enable globally or per-streamer |
| **Point Goals** | Notifies when reaching a point threshold | Per-streamer rules with threshold, can be one-time or recurring |
| **Stream Online** | Notifies when a streamer goes live | Enable globally or per-streamer |
| **Stream Offline** | Notifies when a streamer goes offline | Enable globally or per-streamer |

#### Point Goal Rules

Point notification rules are stored in the database with the following structure:

```
PointRule
├── id: int64
├── streamer: string
├── threshold: int
├── deleteOnTrigger: bool
└── triggered: bool
```

- **Threshold crossing**: Notifications only fire when points cross the threshold (going from below to above)
- **Recurring rules**: If `deleteOnTrigger` is false, the rule resets when points drop below the threshold
- **One-time rules**: If `deleteOnTrigger` is true, the rule is deleted after triggering

### API Endpoints (Notifications)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/notifications` | GET | Notifications management page |
| `/api/notifications/config` | GET | Get notification configuration |
| `/api/notifications/config` | POST | Update notification configuration |
| `/api/notifications/channels` | GET | List available Discord channels |
| `/api/notifications/points` | GET | List point notification rules |
| `/api/notifications/points` | POST | Add a point notification rule |
| `/api/notifications/points/{id}` | DELETE | Delete a point notification rule |

---

## Security Considerations

- OAuth tokens stored locally can access account
- Never log or expose tokens
- SSL verification should remain enabled
- Bot detection possible via integrity token
- Uses TV client to appear as legitimate device
- Discord bot tokens should be kept secret and not shared

### Config File Hardening (`internal/config`, `internal/util`)

`config.json` may contain the Discord bot token and is rewritten at runtime
by dashboard saves (settings, auto-redeem, policy, health), so `SaveConfig`
writes it via `util.WriteFileAtomic` — temp file in the same directory +
rename (the same swap pattern the self-updater uses for the binary) — with
owner-only `0600` permissions. `LoadConfig` migrates a pre-hardening `0644`
file to `0600` on load (best-effort chmod; a failure only warns), so the fix
applies on the first start of the new code rather than the next save.

`DISCORD_BOT_TOKEN` env var optionally supplies the token instead of the
file (same env-over-config, never-persisted precedence as `DASHBOARD_HOST`):
while set, `Config.DiscordTokenFromEnv` is flagged, the Settings UI neither
receives nor overwrites the real value, and **every** `SaveConfig` — from
any dashboard save path, not only Discord settings — serializes an empty
`botToken`, permanently clearing the on-disk copy. Removing the env var
later does NOT restore the file value; the token must be re-entered. This is
deliberate (the environment is the source of truth while set) and documented
in the README.

### Auto-Update Integrity (`internal/updater`)

Binary self-update is fail-closed: `verifyChecksum` refuses the install when
the release has no `checksums.txt` asset, when the checksums file cannot be
downloaded, when it has no entry for the platform asset, or when the sha256
mismatches — an unverified binary is never swapped in (`replaceExecutable` is
only reached after verification succeeds). The miner itself stays best-effort:
a refused/failed update is logged, recorded as an `update_failed` event, and
surfaced once per version via the Discord system channel
(`Options.NotifyFailure` → `Manager.NotifyUpdateFailed`), and mining continues
on the current version. The Release workflow publishes `checksums.txt`
(sha256sum over all binaries) with every release.

The container image also defines a `HEALTHCHECK` executing
`/twitch-miner-go -healthcheck` (scratch has no shell): it loads the same
config, probes `GET /api/status` on the resolved dashboard address (loopback
for wildcard binds), attaches `DASHBOARD_USERNAME`/`DASHBOARD_PASSWORD` when
set, and exits 0/1. With `enableAnalytics=false` it reports healthy.

### Dashboard Security Model (`internal/web/security.go`)

The dashboard is an admin surface (it can spend channel points and change
persisted settings), so the web server enforces a fail-closed exposure model:

**Bind resolution.** Default bind is `127.0.0.1` (config default in
`DefaultAnalyticsSettings`). Effective host = `DASHBOARD_HOST` env var if set,
else `analytics.host` from config.json. The env override is never persisted
back into config.json. The Docker image sets `DASHBOARD_HOST=0.0.0.0` so
published container ports keep working; actual LAN exposure is then governed
by the container runtime's port publishing (Docker `-p`, TrueNAS SCALE /
unraid app UI).

**Startup gate.** `Server.Start()` returns an error — and `cmd/miner` exits —
when the resolved bind is non-loopback and `DASHBOARD_USERNAME`/
`DASHBOARD_PASSWORD` are unset, unless `DASHBOARD_INSECURE_NO_AUTH=true`
explicitly (and loudly, via a startup warning) opts out. Loopback binds never
require auth.

**Middleware chain** (outermost first), built in `Server.handler()`:

1. `securityHeadersMiddleware` — `X-Content-Type-Options: nosniff`,
   `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`, and a CSP
   (`'self'` + `'unsafe-inline'` for the inline template scripts and vendored
   htmx/ApexCharts; `img-src` additionally allows `https:` for Twitch CDN
   art; `connect-src 'self'` covers fetch + SSE).
2. `basicAuthMiddleware` (only when credentials are configured) — HTTP Basic
   over the entire mux, constant-time credential comparison
   (`crypto/subtle`).
3. `csrfProtectMiddleware` — same-origin enforcement for every non-GET/HEAD/
   OPTIONS request: `Sec-Fetch-Site` when present must be
   `same-origin`/`none`; otherwise `Origin` (then `Referer`) must match the
   request `Host` or an entry in `DASHBOARD_TRUSTED_ORIGINS`
   (comma-separated, for reverse proxies that rewrite `Host`); requests with
   none of these headers pass (non-browser clients — browsers always attach
   origin provenance to cross-site state-changing requests). `Origin: null`
   is rejected. GETs — including the SSE stream — are untouched.

**Server timeouts.** `ReadHeaderTimeout: 10s`, `IdleTimeout: 120s`,
`MaxHeaderBytes: 64KB`. `ReadTimeout`/`WriteTimeout` are deliberately unset:
`/api/miner-status/stream` is a long-lived SSE response a blanket deadline
would kill. The localhost-only debug server gets the same header/idle
timeouts.
