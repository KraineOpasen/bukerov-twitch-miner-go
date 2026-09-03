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
| Twitch IRC | `irc.chat.twitch.tv:6697` (TLS) | Chat presence and mentions |
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
├── health/                     # Health Center (see "Health Signals")
│   ├── center.go               # Signal store/snapshot (ok/degraded/failed/idle/stalled/unknown)
│   ├── canary.go               # Watch-transport accrual canary
│   ├── progress.go             # Drop-progress watchdog (stall detection + recovery pipeline)
│   └── avoid.go                # Temporary channel-avoid list used by recovery stage 6
│
├── policy/                     # Campaign policy engine (see "Campaign Policy Engine")
│   └── policy.go               # Pure, deterministic campaign ranking + feasibility
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
│   ├── file.go                 # WriteFileAtomic (temp file + fsync + rename swap)
│   ├── format.go               # Number and time formatting (FormatNumber, FormatDuration, FormatTimeAgo)
│   └── random.go               # Random ID generation (RandomHex, DeviceID)
│
├── i18n/                       # Dashboard localization
│   ├── i18n.go                 # Locale catalog loading and lookup
│   └── locales/                # Embedded JSON message catalogs (en, ru)
│
├── logger/                     # Logging
│   └── logger.go               # Structured logging setup
│
├── updater/                    # Binary self-update (see "Auto-Update Integrity")
│   ├── updater.go              # Release check, fail-closed verification, binary swap
│   ├── stable.go               # Strict stable release/asset/identity policy
│   ├── provenance.go           # Sigstore/SLSA producer and source verification
│   └── recovery.go             # Durable two-slot stable recovery and re-exec
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
| `claimDropsOnStartup` | boolean | false | Deprecated compatibility no-op. Drop rewards are checked and claimed unconditionally during the normal first full sync (see "Drop Claiming Flow"); this flag has no behavioral effect and is retained only so legacy config.json files still parse |
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
| `PlaybackAccessToken` | `ed230aa1e33e07eebb8928504583da78a5173989fadfb1ac94be06a04f3cdbe9` | Get stream playback token (requires `platform: "web"` variable) |
| `VideoPlayerStreamInfoOverlayChannel` | `198492e0857f6aedead9665c81c5a06d67b25b58034649687124083ff288597d` | Get stream info |
| `ClaimCommunityPoints` | `46aaeebe02c99afdf4fc97c7c0cba964124bf6b0af229395f1f6d1feed05b3d0` | Claim bonus points |
| `CommunityMomentCallout_Claim` | `e2d67415aead910f7f9ceb45a77b750a1e1d9622c936d832328a0689e054db62` | Claim moments |
| `DropsPage_ClaimDropRewards` | `a455deea71bdc9015b78eb49f4acfbce8baa7ccbedd28e549bb025bd0f751930` | Claim drops |
| `ChannelPointsContext` | `374314de591e69925fce3ddc2bcf085796f56ebb8cad67a0daa3165c03adc345` | Get channel points |
| `JoinRaid` | `c6a332a86d1087fbbb1a8623aa01bd1313d2386e7c63be60fdb2d1901f01a4ae` | Join a raid |
| `Inventory` | `d86775d0ef16a63a33ad52e80eaff963b2d5b72fada7c991504a57496e1d8e4b` | Get user inventory |
| `MakePrediction` | `b44682ecc88358817009f20e69d75081b1e58825bb40aa53d5dbadcc17c881d8` | Place prediction bet |
| `ViewerDropsDashboard` | `5a4da2ab3d5b47c9f9ce864e727b2cb346af1e3ea8b897fe8f704a97ff017619` | Get drop campaigns |
| `DropCampaignDetails` | `039277bf98f3130929262cc7c6efd9c141ca3749cb6dca442fc8ead9a53f77c1` | Get campaign details |
| `DropsHighlightService_AvailableDrops` | `782dad0f032942260171d2d80a654f88bdd0c5a9dddc392e9bc92218a0f42d20` | Get available drops |
| `GetIDFromLogin` | `94e82a7b1e3c21e186daa73ee2afc4b8f23bade1fbbff6fe8ac133f50a2f58ca` | Get user ID from username |
| `ChannelFollows` | `eecf815273d3d949e5cf0085cc5084cd8a1b5b7b6f7990cf43cb0beadf546907` | Get followed channels |
| `ContributeCommunityPointsCommunityGoal` | `5774f0ea5d89587d73021a2e03c3c44777d903840c608754a1be519f51e37bb6` | Contribute to goals |
| `RedeemCustomReward` | `d56249a7adb4978898ea3412e196688d4ac3cea1c0c2dfd65561d229ea5dcc42` | Redeem custom channel-points reward (renamed server-side from `RedeemCommunityPointsCustomReward`) |
| `DirectoryPage_Game` | `cb5dc816e139dcb8a118f14b4b677d59abc224a4b016c4bc2bb00a47fe0ddec4` | List live channels in a game directory (drops-only via `options.systemFilters: ["DROPS_ENABLED"]`); hash rotates every few months — track DevilXD/TwitchDropsMiner's constants.py |
| `DirectoryGameRedirect` | `1f0300090caceec51f33c5e20647aceff9017f740f223c3c532ba6fa59f6b6cc` | Resolve a game display name to its directory slug (`game(name:) { id slug }`) |

#### Stale-hash handling (`PersistedQueryNotFound`)

Twitch periodically rotates the persisted-query hashes (and occasionally the
variable shape) of these operations server-side. When the hash the code ships no
longer matches, Twitch answers **HTTP 200** with an
`errors[].message = "PersistedQueryNotFound"` body and no `data`. The GQL client
(`internal/twitch/client.go`) handles this so a Twitch-side rotation degrades
gracefully instead of corrupting local state:

- **Per-operation client-ID fallback.** Generic GQL requests are tried against an ordered
  list of public client IDs (`constants.GQLClientIDFallbacks`: TV → Browser →
  Mobile). The order is per-operation: the last client ID that worked for that
  operation is tried first (cached in a mutex-guarded map, `opClientID`), then
  the promoted default, then the rest. A client ID that resolves a
  `PersistedQueryNotFound` is cached for the operation and promoted to the global
  default, logged **once** on the actual promotion (no per-request spam, even
  under concurrent recovery). The active default is surfaced in the Health Center
  as the *Active GQL Client ID*.
- **Distinct error, not "streamer does not exist".** When *every* candidate
  client ID still returns `PersistedQueryNotFound`, the hash itself is stale:
  `postGQLRequest` returns `twitch.ErrPersistedQueryNotFound` (one `ERROR` log naming
  the operation and the count of client IDs tried). Callers treat this as a
  temporary outage — they keep the last-known channel points, tracked drop
  campaigns and online flag (`CheckStreamerOnline` does not flap a streamer
  offline on this error), and never report the channel as non-existent. The fix
  for a confirmed rotation is a hash update in `internal/constants/gql.go`
  (cross-check DevilXD/TwitchDropsMiner's `constants.py`).
- **Robust parsing.** Empty bodies, malformed JSON, missing `data`, and Twitch
  error responses are all handled with `, ok` type assertions and explicit
  guards — a changed Twitch response shape yields a clean error, never a panic.
- **Side-effect exception.** `ClaimCommunityPoints` does not use the generic
  substring detector or transient retry loop. Its client-ID fallback is allowed
  only for an HTTP-200 response whose every error has exact code
  `PERSISTED_QUERY_NOT_FOUND` and whose `data` is absent/null; all other ambiguous
  outcomes fail closed. See *Bonus claim arbitration* below.

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
| `predictions-user-v1` | `prediction-result` | Terminal result for a locally tracked confirmed round (tracked-only, at-most-once admission) |
| `predictions-user-v1` | `prediction-made` | Confirm bet placed |
| `community-points-channel-v1` | `community-goal-*` | Update/contribute to goals |

### Connection Management
- Send PING at configured interval (default 27s) with ±2.5s random jitter
- Reconnect if no PONG received within 5 minutes
- Auto-reconnect on disconnect after `rateLimits.reconnectDelay` seconds
  (configurable 30-300, default 60)

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

#### Bonus claim arbitration

PubSub `claim-available`, the periodic fallback poll, and full Channel Points
context hydration share one process-local ledger owned by each `Streamer`.
Entries are keyed by the exact authoritative claim ID and retain terminal
tombstones for the Streamer's lifetime. Exactly one caller may transition an ID
to `in_flight`; duplicates are benign and perform no mutation or success event.
Accepted and authoritative rejected results are terminal. An ambiguous remote
outcome is quarantined as indeterminate because Twitch idempotency is not
assumed.

`ClaimCommunityPoints` uses a side-effect-specific transport: redirects and the
generic connection/429/5xx retry loop are disabled. Network/read failures,
non-200 statuses other than the first HTTP 401, malformed results, and
conflicting GraphQL errors are indeterminate and are not replayed. Exact
APQ-not-found responses may try the bounded public client-ID list. The first
HTTP 401 is the explicit pre-execution authentication exception and may perform
one credential-recovery replay; a second 401 stops that transport cycle. A
logical claim gets at most one later attempt, and only when
the previous attempt was proved not to execute and a newer fully applied
Channel Points context still advertises the same ID. The Streamer lock is never
held across network I/O. At most the fresh accepted owner emits a local success
event; authoritative `points-earned` events remain the source of balance/history.

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
   Success: HTTP 204 No Content — and ONLY 204
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

#### Beacon Status Contract

**`HTTP 204 No Content` is required for a credited minute-watched beacon — and it
is not by itself proof of credit.** Twitch answers a stale or malformed
minute-watched payload at the transport layer — commonly `HTTP 200` — *without
counting the watch*, so treating any non-204 status as success produces a silent
false-positive: watched minutes, slot `delivery_success` records and watch-time
fairness credit are all booked for a view Twitch never granted, while nothing in
the logs looks wrong.

A 204 means only that the beacon was accepted. Whether the watch was actually
credited is observable solely through real `WATCH` points events and server-side
balance growth — the same honest limitation the health canary documents.

Every non-204 status is a bounded `StageBeacon` failure carrying the status and
the `beacon_http_<status>` error code; it never increments delivered minutes,
never counts as a slot delivery success, never triggers `onMinuteWatched`, and
never writes watch-time fairness credit.

The beacon carries no credential material: only `Content-Type` and `User-Agent`
are set, and the beacon POST refuses to follow redirects (a redirected target
could be cross-origin, downgrade HTTPS, or — for 307/308 — replay the body to a
third party). No OAuth, `Client-Id`, `Device-Id`, cookie, `Origin` or `Referer`
header is sent to the spade endpoint.

#### Minute-Watched Payload

Every property below is required and its **JSON type is part of the contract**.
`broadcast_id`, `channel_id` and `game_id` are JSON *strings* while `user_id` is a
JSON *number*; that asymmetry is deliberate. `game`/`game_id` are always present
(`""` when the game is unknown) rather than omitted, and the `false`-valued
booleans are always serialized.

`client_time` is stamped once when the payload is built and replayed unchanged by
every beacon sent from that payload — it is bound to the playback session, not to
the individual send. That lifetime is taken from the reference implementation,
which caches the whole payload (and therefore its `client_time`) per stream object
and replays identical bytes for every beacon; Twitch credits that.

It also has to be that way given where `client_time` lives. Because the timestamp
is part of the *published* payload, re-stamping it per send would mean
re-publishing the payload each minute; every re-publish bumps the playback-session
generation, and the sender's coherence gate suppresses any send whose captured
generation no longer matches — so it would silently turn every beacon into a
stale-session no-op. (Stamping the timestamp in the sender instead, outside the
published payload, would be a different design; it is not adopted, because the
reference behaviour shows a session-bound value is what earns credit.)

An empty or non-numeric authenticated user id is refused outright: no payload is
built, so the sender fails closed at its session-snapshot gate before any spade
request. A `user_id` of `0` is never sent.

```json
[{
    "event": "minute-watched",
    "properties": {
        "broadcast_id": "789012",
        "channel_id": "123456",
        "channel": "streamer_name",
        "client_time": "2026-09-03T16:24:44.000Z",
        "game": "Game Name",
        "game_id": "12345",
        "hidden": false,
        "is_live": true,
        "live": true,
        "logged_in": true,
        "minutes_logged": 1,
        "muted": false,
        "player": "site",
        "user_id": 456789
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

- **Phase A — configured selection**: the priority/rotation logic below picks
  up to two channels from the configured streamer list (direct priority pick
  when ≤2 online, fair rotation with a DROPS/STREAK boost when more). Hard
  boost classes remain outermost; Campaign Policy bounded semantic utility
  orders comparable drop contenders before persisted-deficit/recency
  tie-breaking.
- **Phase B — cross-source arbitration**: candidate sources (directory
  discovery today) are layered on top. A candidate fills any free slot;
  otherwise it may displace the lowest-ranked configured occupant it strictly
  out-ranks. Continuity protection never inverts a strictly stronger hard
  class. A channel already holding a slot never gets a second one. Ranking
  (high→low): channel-restricted drop → in-progress watch streak
  → active drop → fair-rotation/priority pick. With no candidate sources
  Phase B is a pure pass-through, so single-list behavior is unchanged. Within
  an equal drop class, Campaign Policy compares the primary semantic class,
  then at most one best distinct feasible secondary campaign, before persisted
  watch-time deficit; equal bounded utility retains deficit fairness and the
  deterministic login tie-break.

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

#### Crash Recovery Policy

`MinuteWatcher.Start` spawns exactly one goroutine (`loop`) per instance, and
neither it nor anything in its call chain installs a local `recover()` —
verified: no `recover()` exists anywhere in `internal/watcher`, nor in
`internal/miner`'s production code that constructs or drives it
(`startMining`'s `m.watcher.Start(ctx)` is the sole call site). This is a
deliberate **crash-only** policy, not an oversight: an unhandled panic in
`loop` is caught nowhere, so Go's runtime default applies — it terminates the
whole process, not just the watch loop. Recovering locally was rejected
because a panic there may mean loop-owned state (`rotation`, `lastSlots`,
`sessionConverge`, and similar fields documented as touched only by this
goroutine) is no longer trustworthy; letting the loop continue past that
risks mining against corrupted state. **Do not add a `recover()` around the
watch loop** — that would undermine this policy, not harden it.

The policy is workable because the loop holds no long-lived worker resources
a crash would leak: each tick's slot allocation (`arbitrate`) builds `slots`
fresh from `w.streamers`/`w.rotation`, never a persistent per-channel worker
pool. A restart simply constructs a new `*MinuteWatcher` and starts selection
over; nothing needs reconciling.

Bringing the process back up after a crash is an **external deployment**
concern — `cmd/miner` and `internal/app` contain no self-restart or re-exec
logic; an unrecovered panic simply ends the process. This repository's own
`docker-compose.yml` sets `restart: unless-stopped` (also required for the
auto-update self-exit-then-relaunch flow), which does cover this case *for
that specific file*. That is not a general property of "Docker" or any other
platform — any other deployment (bare binary, systemd unit, a different
container/App definition) must supply its own equivalent restart policy for
the process to come back after a crash.

Two related hardening ideas are intentionally deferred, not bugs:

- **R9-F1 (`Start` idempotence guard):** `Start` has no guard against being
  called twice on the same instance — a second call would silently overwrite
  `w.ctx`/`w.cancel`/`w.loopDone` and leak the first `loop()` goroutine.
  Unreachable in production today: the durable lifecycle controller
  (`internal/lifecycle.Controller`) never restarts an existing generation —
  its `Config.Factory` (wired in `internal/app.Build` as `minerFactory`)
  constructs a brand-new `*miner.Miner`, and therefore a brand-new
  `*MinuteWatcher`, for every generation, so `Start` runs at most once per
  instance. `DEFERRED_HARDENING`.
- **R9-F3 (broker snapshot staleness signal):** `BrokerSnapshot` carries an
  `EvaluatedAt` timestamp but nothing actively signals a consumer when a
  snapshot has gone stale. `DEFERRED_HARDENING`.

### Priority System

Maximum 2 streams watched simultaneously (`constants.MaxSimultaneousStreams`),
allocated by the unified slot broker (see *Watch Slot Architecture*).

**2 or fewer online streamers:** all of them are watched; the priority list below picks which ones fill the (at most 2) watch slots, same as always:

| Priority | Behavior |
|----------|----------|
| `STREAK` | Prioritize an eligible identified-broadcast streak pursuit until authoritative grant or the exact 20 continuous-delivered-minute cap (> 30 min since offline where that existing admission gate applies) |
| `DROPS` | Prioritize streamers with active drop campaigns |
| `SUBSCRIBED` | Prioritize subscribed channels (higher tiers first) |
| `ORDER` | Follow order in streamers list |
| `POINTS_ASCENDING` | Lowest points first |
| `POINTS_DESCENDING` | Highest points first |

**More than 2 online streamers:** a fixed priority pick would starve every other online channel indefinitely, so the watched pair instead rotates fairly across all online streamers. See `internal/watcher.selectRotating` (and `store.go` for persistence) for the full algorithm:

- **Persisted fairness on the broker tick:** every ordinary broker evaluation ranks each online streamer by accumulated watch minutes over the trailing 8-hour window, persisted in SQLite (`watch_time_events`, module `watch_time`, survives container restarts), and gives the base slots to the two with the *least* accumulated time. There is no randomized or fixed rotation residence timer. Ties (including cold start) use in-memory recency and then normalized login, so candidate permutation cannot change the result. Whoever is watched accumulates minutes and becomes less owed, surfacing every valid contender without an in-memory cursor or parity special case.
- **Priority as a boost, not exclusivity:** on top of the weighted base pair, an online streamer with an active drop (`DROPS`) or a pursuit-eligible watch streak (`STREAK`) can take over one seat without altering persisted weights. Watch Streak continuity across a base-pair reconciliation covers the zero-minute `ELIGIBLE` bootstrap needed to bank the first delivered interval and the genuinely `PURSUING` state; it ends on a bound authoritative grant, the exact 20-minute `TIMED_OUT_UNKNOWN` transition, lost eligibility, or a strictly stronger hard/semantic contender. An ordinary active or channel-restricted drop has no equal-class continuity exception: strictly stronger current facts still win, while a full hard/semantic tie converges to persisted-deficit fairness instead of preserving a previous latch. Hard restricted/streak/drop class is compared first. Comparable drop contenders then compare Campaign Policy utility lexicographically: primary `SemanticClass`, presence of at most one qualifying distinct secondary campaign, then that secondary's `SemanticClass`. Recency and normalized login finish deterministic ties without granting a third slot.
- **Continuous-watch accounting:** `Stream.MinuteWatched` measures *continuous successfully delivered* watched minutes for one exact non-empty `BroadcastID`, not broadcast age, uptime, discovery age, scheduler ticks, failed reports, or wall-clock dwell. A real slot loss resets only the continuous counter and its report anchor. It preserves the grant ledger, broadcast binding, and same-broadcast timeout latch, so reacquiring a timed-out broadcast never opens a second 20-minute window. A transient status blip with the same `BroadcastID` is not a new broadcast; only a genuinely changed non-empty `BroadcastID` re-arms a broadcast-specific pursuit.
- **Single watch-streak owner:** `Stream` derives every direct-selection and broker-protection verdict from the same state: `ELIGIBLE → PURSUING → GRANTED | TIMED_OUT_UNKNOWN`. Zero, 7, 8, 15, and 19 delivered minutes remain eligible/pursuing; 15 minutes is diagnostic only and causes no transition. Exactly 20 minutes without a proven bound grant latches `TIMED_OUT_UNKNOWN`, releases streak priority, and persists that outcome through the existing atomic streak cache. Timeout means outcome unknown, never failed, missed, impossible, or inactive. A bound authoritative grant dominates timeout; a late grant is accepted once without reopening pursuit.
- **Grant attribution and replay:** ordinary `WATCH` is delivery evidence only and can never grant a streak. An authoritative `WATCH_STREAK` is admitted exactly once by its canonical PubSub event fingerprint. When independent evidence proves a `BroadcastID`, the grant is `GRANTED` for that broadcast; otherwise it is persisted and counted explicitly as `GRANTED_UNBOUND`, never guessed onto the currently observed broadcast and never allowed to end, re-arm, or time out that broadcast's pursuit. Terminal broadcast facts and exact replay identities do not expire by wall-clock age; a proven new `BroadcastID` is the only re-arm signal. WebSocket replay suppression uses the full canonical topic+payload fingerprint, so distinct back-to-back point events are not collapsed.
- **Watch-streak pursuit diagnostics:** the watcher logs pursuit once and the exact 20-minute release once. Release is outcome-neutral (`TIMED_OUT_UNKNOWN`); WATCH-credit count is diagnostic evidence only, and neither the historical 7-minute hint nor the 15-minute diagnostic reference controls eligibility, protection, displacement, or timeout.
- **Bounded streak deferral:** when the current fair pair is about to lose an in-progress streak member, the loop may set one explicit `deferUntil = now + 2 min` for that pair approach. Re-evaluation cannot extend the deadline; expiry forces reconciliation, and a member that goes offline or loses protection leaves immediately. `PairSince` changes only when pair membership actually changes. This best-effort protection does not cover imminent drop-campaign completion and never overrides a strictly stronger hard class.
- **Retired configuration compatibility:** old `config.json` files containing `rateLimits.rotationInterval`, `rotationIntervalMinMinutes`, or `rotationIntervalMaxMinutes` still decode because unknown JSON keys are accepted. Those values have no runtime owner or effect and `SaveConfig` never serializes them. Runtime Settings, the Settings page, diagnostics, and the sidebar expose no retired fields or future-rotation projection.
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
   ├── Validate terminal payload (WIN/LOSE/REFUND)
   ├── Admit at most once per tracked confirmed round
   └── Update statistics (admitted delivery only)
```

### Terminal Result Admission (tracked-only)

`predictions-user-v1` is an **account-scoped transport** topic: a
`prediction-result` frame on it is transport evidence, not by itself business
authority. Terminal Prediction business handling is authoritative only for a
**locally tracked, bet-confirmed round** owned by `pubsub.WebSocketPool`:

- a structurally valid first terminal result (`WIN`/`LOSE`/`REFUND` with a
  coherent `points_won` payout) for a tracked confirmed `event_id` is admitted
  **at most once during the process lifetime of that tracked round**;
- duplicate, replayed, or conflicting terminal results after first admission
  are ignored for terminal business side effects (history/compensation, the
  event ring, the `BetResult` emission, and the WIN/LOSE analytics
  annotation);
- malformed or unsupported results are rejected without consuming the
  admission — a later valid result for the same round can still win (a
  `WIN` requires a coherent non-negative integral `points_won`; for
  `LOSE`/`REFUND` an absent or JSON-null `points_won` counts as the
  canonical zero payout, while a contradictory numeric value is rejected);
- untracked / never-tracked / post-cleanup results produce **no** terminal
  WIN/LOSE analytics annotation (owner decision: tracked-only terminal
  telemetry);
- `prediction_bets` keeps its separate durable sink-local idempotency
  (`UNIQUE(event_id)` + `INSERT OR IGNORE`) as defense in depth;
- none of this establishes durable cross-process exactly-once semantics: the
  admission gate does not survive a restart, and an individual sink can still
  fail after admission.

`prediction-made` handling is unchanged by this contract.

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
├── hasPreconditionsMet: bool | null (null is distinct from false and true)
├── dropInstanceId: string (null until started)
├── isClaimable: bool
├── isClaimed: bool
├── startAt: datetime
└── endAt: datetime
```

### Drop Claiming Flow

```
1. Sync Campaigns (startup; every 60 minutes by default; manual/config wake)
   ├── GET ViewerDropsDashboard (explicitly non-active summaries are rejected;
   │   a missing summary status fails open to detail/window validation; an
   │   explicit JSON null dropCampaigns listing is UNKNOWN — the sync
   │   continues and the inventory recovery in step 2 still applies, but the
   │   listing carries no authority; missing/wrong-type/malformed listings and
   │   top-level GQL errors remain errors — see "Dashboard-listing authority")
   ├── GET DropCampaignDetails for each
   ├── Backfill campaign date window (startAt/endAt) from the dashboard
   │   summary when the details response omits it, then recompute the
   │   date-window match, so an active campaign isn't dropped as "outside its
   │   date window" while a genuinely-expired details window is still honored
   └── Filter by date range

   (One concise INFO line is logged per sync — dashboard count, recovered-
   from-inventory count, and tracked count — and the same figures plus the
   tracked campaign list are exposed at GET /debug/snapshot under "drops", so
   an empty Drops page is diagnosable without -debug. For an UNKNOWN listing
   the summary instead reports tracked / recovered-from-inventory /
   kept-from-last-known counts — see "Dashboard-listing authority".)

2. Sync Inventory
   ├── GET Inventory
   ├── Observe raw self.isClaimed sightings into the drop skip ledger (E3 —
   │   read straight from the decoded response maps, BEFORE anything below
   │   can strip a drop from a Campaign; see "Drop Skip Ledger" below)
   ├── Match drops to campaigns
   ├── Update progress
   └── Recover any dropCampaignsInProgress campaign missing from the
       dashboard/details path (build it straight from the inventory entry, no
       date-window gating), so a campaign Twitch is actively crediting always
       appears on the Drops page even when its DropCampaignDetails fetch
       returned nothing

2b. Apply Claim History (see "Claim History Check" below — against the proven
    Twitch response shape this pass alone can never actually remove a drop;
    the drop skip ledger is what closes that gap)
   ├── GET Inventory (gameEventDrops: account-wide granted rewards)
   ├── Build an evidence-ranked RewardIdentity per record (an instance,
   │   benefit, or campaign+drop composite ID when Twitch supplies one, else
   │   a name-only fallback)
   └── Strip a drop only on a positive, provable identity match (MatchIdentity
       == Confirmed, or the strict unique-name+overlapping-window upgrade);
       everything else — which is every record the proven `gameEventDrops`
       shape actually produces — fails open and the drop is retained

2c. Reconcile Drop Skip Ledger (self-heal, once per full sync — see "Drop
    Skip Ledger" below)
   ├── Runs on the raw, still-unfiltered candidate set, immediately after the
   │   catalog is recorded
   └── Writes to the ledger only; never mutates a campaign

3. Check Claimable
   ├── dropInstanceId != null
   ├── isClaimed == false
   └── currentMinutesWatched >= requiredMinutesWatched

4. Claim Drop
   ├── POST DropsPage_ClaimDropRewards
   ├── Mark as claimed
   └── Observe the authoritative outcome into the drop skip ledger (E1 fresh
       accept / E2 already-claimed — see "Drop Skip Ledger" below)

5. Broker-Facing Assignment (updateStreamerCampaigns — every full AND
   lightweight sync that publishes; a sync that leaves the published pool
   untouched — an UNKNOWN dashboard listing with no fresh inventory evidence,
   or a no-change light sync — skips it, since assignments are already
   current)
   └── Filter each campaign's drops against the skip ledger (Decide) before a
       streamer may be assigned it, on a CLONE only — the tracked pool itself
       (Campaigns(), the catalog, every published Campaign) always stays the
       unfiltered set
```

### Full campaign-discovery scheduling

`DropsTracker.loop` remains the sole background owner of full campaign-sync
wakes. It performs one immediate startup sync, then creates a fresh timer for
the current `campaignSyncInterval` after each run. Runtime filter changes and
the manual Sync action use the single buffered `campaignResync` wake; `SyncNow`
uses the same full-discovery pipeline directly. Every path is serialized by
`fullSyncMu`, so no full sync overlaps another, and an early wake restarts the
ordinary interval rather than shortening the steady cadence. Newly active
campaigns are discovered by the next ordinary or explicitly triggered full
sync. There is no per-campaign timer, future-boundary scheduler, persisted timer
state, accelerated polling cadence, or replacement discovery loop. Campaign
date windows remain start-inclusive and end-exclusive.

### Lightweight progress sync

The ordinary full-sync cadence above remains `campaignSyncInterval` minutes
(60 by default) because a full sync is expensive: a
`ViewerDropsDashboard` listing plus one
`DropCampaignDetails` fetch per listed campaign plus several `Inventory`
reads. On its own the ordinary full sync would leave dashboard/Drops-page
progress up to a full interval stale — a
campaign shown at 58% (140/240 min) while Twitch already credits ~69%.

To keep the displayed progress within a minute or two of Twitch's real
progress, `DropsTracker` runs a second, much cheaper loop
(`progressLoop`/`syncProgress`) on `dropProgressSyncInterval` minutes (2 by
default, range 1-60):

```
Progress sync (dropProgressSyncInterval, or on demand)
   ├── GET Inventory   (single query; no dashboard listing, no per-campaign
   │                    DropCampaignDetails fetches)
   ├── For each already-tracked campaign the inventory reports Drop state for:
   │   clone it, refresh currentMinutesWatched and, when explicitly reported as
   │   Boolean, hasPreconditionsMet from the inventory `self` data; drop
   │   claimed/out-of-window drops
   └── Republish the campaign pool (fresh objects, swapped under lock so the
       Drops page and directory discovery keep reading immutable published
       campaigns) only when tracked publication state changed semantically
```

It never discovers new campaigns, claims Drops, or owns the full-sync campaign
filters. On campaigns the full sync already published, a Drop-set/count/identity
change, a `CurrentMinutesWatched` change, or a semantic `HasPreconditionsMet`
tri-state change (`nil`, explicit `false`, or explicit `true`) republishes the
shared immutable pool. A `HasPreconditionsMet`-only publication increments
`Revision` exactly once, records `UpdateSource=light_sync`, and re-points
streamers using the fresh published `Campaigns()` snapshot. Repeated
semantically identical observations do not churn the pool, revision, source,
or streamer pointers; an omitted `hasPreconditionsMet` observation does not
erase a previously known value.

`HasPreconditionsMet` is publication state here, not a local interpretation of
watch-progress or broker eligibility. In particular, `nil` is not coerced to
either Boolean value and explicit `false` is not classified as `IMPOSSIBLE`.
A newly seen campaign still requires a full sync for discovery and is found on
the ordinary interval or an explicit full-sync wake.
The watcher calls `DropsTracker.TriggerProgressSync` after every successfully
reported watched minute, so a watched minute is reflected on the Drops page
within seconds rather than waiting out the interval.

### Progress-sync freshness (stale light-sync results are discarded)

`syncProgress` snapshots the published campaign pool and its `Revision` under
the tracker's read lock, then performs its `Inventory` request without
holding the lock (no network call ever runs under `d.mu`). Because the full
sync can run concurrently and replace the pool in between, the response that
comes back may describe a campaign set a newer full sync has already
superseded — an unguarded republish would resurrect a removed campaign, drop
one the full sync just added, or revert streamer assignments, silently
undoing the full sync's own result.

Every outcome a light sync can report — changed tracked state, an unchanged
observation, or a valid empty/no-in-progress inventory — is therefore
conditional on the revision it observed: immediately before publishing
(or, for an unchanged/empty result, before recording the observation at
all), the tracker re-checks that its published `Revision` still equals the
one captured at the start of the sync, atomically with the check itself. If
the revision has moved on, the result is discarded in full:

- the campaign pool, `Revision`, `BackendUpdatedAt`, and `UpdateSource` are
  left exactly as the newer (superseding) sync set them;
- streamers are not re-pointed;
- `ProgressLastSyncAt`/`ProgressLastError` are not touched — a discarded
  result must never look like a fresh observation of the newer pool;
- at most a DEBUG diagnostic is logged; the sync returns without retrying.

The subsequent streamer re-point is fenced separately against a newer
publication. If a light-sync assignment pass captured an older `Revision`, it
is discarded before applying; if the newer full-sync publication lands after
that check, serialized re-pointing makes the newer pass run last. An older
light-sync pass therefore cannot overwrite a newer full-sync assignment.

The next scheduled (or on-demand) light sync simply observes whatever pool is
current by then. Progress can legitimately decrease after a valid Twitch
observation (no local monotonic-max rule), and this guard does not change
that — it only ensures a response is judged against the pool it was actually
taken from.

### Dashboard-listing authority (explicit null is UNKNOWN, never authoritative zero)

The full sync classifies the `ViewerDropsDashboard` response's
`data.currentUser.dropCampaigns` value into three distinct states:

- **Explicit JSON array (including `[]`)** — authoritative. `[]` is a genuine
  "no campaigns listed" observation and flows through the normal
  dashboard/details path; campaigns absent from an authoritative listing may
  be removed by the ordinary pipeline.
- **Missing key, wrong type, or malformed campaign elements — and any
  top-level GQL `errors` member** — an error, exactly as before: the sync
  fails the dashboard/details stage and the last-known-good pool is preserved
  (see the next section). This classification is deliberately unchanged (the
  PR #252 response-authority protection).
- **Explicit JSON `null`** — a distinct UNKNOWN/unavailable listing state.
  Production Twitch has been empirically observed (2026-08, authenticated
  read-only canaries across persisted-query hashes, variables, and client
  profiles) answering HTTP 200 with `data` and `currentUser` objects present
  and exactly this null — without top-level errors on all but one tested
  profile (the browser/web profile carried top-level `errors` alongside the
  null; that combined shape is still rejected by the errors gate above). The
  errorless null is **not** an error and **not** an authoritative empty
  listing: the sync continues with an empty dashboard/details-derived set,
  and the existing `syncWithInventory` reconciliation (step 2 of "Drop
  Claiming Flow") still surfaces every in-progress campaign Twitch is
  actively crediting — a bounded, positive-only recovery. "Fresh evidence"
  here means an in-progress entry that yields at least one usable unclaimed
  drop; a fully-claimed entry leaves the campaign's previous version carried
  over while claiming and the drop skip ledger consume the raw entry
  directly.

A null listing alone can therefore never erase last-known campaign state.
Campaigns the inventory positively proves in progress take the normal
pipeline's outcome (refreshed, or removed by claim-history/blacklist/game/
account-link authority); a refreshed campaign rebuilt from a date-less
inventory entry inherits its previous version's known StartAt/EndAt window
(a date-less rebuild cannot zero out good dates, the same rule the drop
catalog applies); previously tracked campaigns with no fresh inventory
evidence are carried over unchanged, because absence from
`dropCampaignsInProgress` is not proof a campaign ended. When there is no
fresh evidence at all, the published pool, `Revision`, `BackendUpdatedAt`,
and `UpdateSource` stay untouched (no republish). Removing a previously
tracked campaign requires an authoritative listing (an explicit array without
it) or campaign-level authoritative evidence (expiry, claim). This mirrors
the existing rules that an omitted `hasPreconditionsMet` observation does not
erase a previously known value, and that a date-less catalog observation
cannot zero out good dates.

An UNKNOWN (null) listing is not recorded as a `SyncStatus.LastError`: the
attempt counts normally (`LastSyncAt`/`Runs` advance, and `LastSuccessAt`
advances iff the subsequent inventory merge succeeds), while
`SyncStatus.DashboardListingUnavailable` is set and `DashboardCampaigns`
reads 0 because nothing was listed — never as an authoritative zero. The
per-sync summary line for this state says the listing was unavailable and
reports tracked/recovered/kept-from-last-known counts; it never claims
"Twitch reports no active drop campaigns", which remains reserved for an
authoritative empty listing.

The UNKNOWN state is a distinct operator-visible state end to end, so
`dashboardCampaigns: 0` can always be told apart from an authoritative zero:

- the Health Center's **Drops Inventory Sync** signal reports `degraded` with
  the stable code `dashboard_listing_unavailable` (detail: inventory
  reconciliation succeeded, N campaign(s) tracked, newly discoverable
  campaigns may be missing) — never ordinary "successful discovery"; an
  actual sync error keeps its `failed`/`sync_error` precedence over the flag;
- the debug snapshot's `drops` section, the manual-sync JSON
  (`POST /api/drops/sync`), and the support bundle's `drops.json syncStatus`
  all carry an explicit `dashboardListingUnavailable` boolean (always
  serialized, so `false` is explicit — key absence never stands in for it),
  crossing the support bundle's typed allowlist as a plain flag with no raw
  Twitch response material, error bodies, or query metadata.

### Full-sync inventory-merge failure preserves the last-known-good pool

The full sync's dashboard/details path (`getActiveCampaigns`) builds fresh
campaign and drop objects that carry no live per-drop progress — the only
write path for a drop's `currentMinutesWatched`/claimability is
`Drop.Update`, called from the `Inventory` merge (`syncWithInventory`) or from
`claimAllDropsFromInventory`. If the `Inventory` request `syncWithInventory`
depends on fails, returns no response, or does not decode to a usable
inventory object, those freshly-built objects never receive that update; the
sync aborts before publishing and keeps the previously published campaign
pool, `Revision`, `BackendUpdatedAt`, and `UpdateSource` unchanged. Active
campaigns are published atomically only after the authoritative inventory merge
succeeds. The sync *attempt* itself is
still recorded — `SyncStatus.LastSyncAt`/`Runs`/
`DashboardCampaigns`/`LastError` update exactly like a dashboard/details-
listing failure (`LastSuccessAt` does not advance) — so the failure is
visible without the last-known-good snapshot ever being replaced. This applies
only to the acquisition
`syncWithInventory` itself performs — `claimAllDropsFromInventory` and
`applyClaimHistory` make their own independent `Inventory` requests and keep
their pre-existing failure handling (best-effort, non-fatal to the sync).

A successfully decoded `Inventory` response that simply reports no
`dropCampaignsInProgress` (a fresh account, or every tracked campaign
genuinely not yet started) is a legitimate observation, not a failure — the
sync continues normally with the dashboard/details-built campaigns.

### Drops Eligibility

A configured streamer has Drop watch-slot authority only when:

- `claimDrops` is enabled;
- the streamer is confirmed online; and
- `Stream.Campaigns` contains an authoritatively assigned campaign with at
  least one non-nil, unclaimed reward whose watched minutes are still below
  its required threshold.

Confirmed online is required to grant a new slot. The broker's existing
bounded online→UNKNOWN liveness-retention rule may preserve an already-held
slot but never creates an assignment or new authority.

`Stream.CampaignIDs` is channel-advertised availability evidence, not an
assignment and never slot authority by itself. Game, ACL, window, account-link,
and other reward eligibility checks are applied by the assignment producer
before it publishes `Stream.Campaigns`. During a transient UNKNOWN availability
result, the existing `CampaignAvailabilityGrace` may retain a previously proven
unfinished assignment; a retained advertised ID without such an assignment
cannot create `active_drop`, `restricted_drop`, or a Drop priority boost.

### Account-Linked Drop Eligibility

Some drop rewards can only be earned once the operator has linked their Twitch
account to the campaign publisher's account (a **direct in-game entitlement**).
Twitch reports this via two fields the existing persisted queries already return
(decoded, not newly requested): the campaign's `self.isAccountConnected` and each
benefit's `distributionType`.

- **Account connection is tri-state** (`models.AccountConnection`, decoded by
  `ParseAccountConnection`): a real boolean `true`/`false` is Connected /
  Disconnected; a null, absent, malformed, or partial value is **Unknown**.
  Unknown always fails open — it is never treated as a proven disconnection.
- **Benefit type is typed** (`models.BenefitType` from `distributionType`):
  `BADGE`, `EMOTE`, `DIRECT_ENTITLEMENT`, or Unknown. Only a direct entitlement
  requires the publisher link (`Drop.RequiresPublisherLink`); badges, emotes, and
  unknown/absent types never do.

`DropsTracker.applyAccountLinkFilter` (`internal/drops/drops.go`) excludes a
reward **only** when `AccountConnection == Disconnected` **and** the reward
requires the publisher link — the single-source-of-truth rule lives in
`eligibility.AccountLinkEligible`, and a skip carries the privacy-safe typed
reason `account_link_required` (no account, publisher, token, or raw-payload
data). The filter runs once per full sync, after the game/blacklist/claim-history
filters (so those observe an unchanged drop set and a stripped reward is still
recorded in the durable "Past" catalog); the lightweight progress sync never
re-filters. Aggregation is reward-level: a mixed campaign keeps its eligible
rewards and stays trackable, while a campaign whose rewards are *all* excluded
becomes untrackable and drops out of the published pool (so the Drops page count
reflects the trackable set). It never alters watch progress, claim history, or
the claim gate.

### Claim History Check

`DropsTracker.applyClaimHistory` (`internal/drops/drops.go`) cross-references
each tracked campaign's drops against the account's Twitch-wide claim history
(`gameEventDrops` in the `Inventory` response) via
`extractClaimedRewards` → `Campaign.ApplyClaimHistoryRecords`
(`internal/models/reward_identity.go`, `internal/models/campaign.go`). This is
the **evidence-aware** replacement for the old lossy game+name key: each
`gameEventDrops` entry is decoded into a `models.ClaimedReward` carrying a full
`RewardIdentity` (game, benefit ID when present, drop/campaign ID, name, and
entitlement window), and `MatchIdentity` only strips a drop on a positive,
provable match — never on a fuzzy name guess.

**This pass is a structural no-op against the currently proven Twitch
response shape, and that is expected, not a bug to "fix" here.**
`extractClaimedRewards` builds every record with `InstanceID=""`, `DropID=""`,
and `EntitlementWindow{}` (`Known=false`) — the proven `gameEventDrops`
contract carries no drop ID (its own `id` field is a per-user *event* ID, not
a campaign's `timeBasedDrop` ID) and no window at all, only occasionally a
benefit ID. Every `Confirmed` path in `MatchIdentity` requires an instance ID,
a benefit ID **plus** two decidable overlapping windows, or a composite ID —
none of which this shape ever supplies. So the strongest outcome an
already-claimed drop can reach is `Ambiguous` (fail open, retained) — see
`claim_history_test.go`'s `TestClaimHistoryFailOpenNoWindow`, which pins this
exact behavior. `ClaimStatus`/`ClaimedDropNames` are still kept on the
(in-memory) campaign list for a future "already claimed" dashboard view, but
in production this pass essentially never actually removes anything, so an
already-awarded reward Twitch re-offers with progress reset to 0 /
`isClaimed=false` (a "ghost") would otherwise be re-farmed forever with
nothing surviving a restart. The **Drop Skip Ledger** below is the durable fix
for that gap, built from evidence this miner itself witnesses rather than from
`gameEventDrops`.

### Drop Skip Ledger

`internal/drops/skipledger.go` is a durable, evidence-ranked ledger fed
**exclusively by evidence this miner itself witnesses** — never by claim
history — that gates only **future broker-facing drop assignment**
(`updateStreamerCampaigns`), never `Drop.CanClaim` or `TwitchClient.ClaimDrop`
themselves. It is what actually stops an already-granted reward Twitch
re-offers ("ghost") from being re-farmed forever, and is scoped per account
(`config.StorageKey()`) via `SkipLedger` / `NewSkipLedger`. See "Drop Skip
Ledger Module Schema" below for the table/index definitions.

**Evidence classes**, ranked strongest to weakest (a row's evidence only ever
strengthens, never weakens):

| class | rank | source |
|---|---|---|
| `claim_accepted`    | 3 | our own `ClaimDrop` returned a fresh, authoritative acceptance (E1) |
| `claim_already`     | 2 | our own `ClaimDrop` returned an authoritative already-claimed reconciliation (E2) |
| `inventory_claimed` | 1 | the raw inventory reports `self.isClaimed == true` (E3) — read straight from the decoded response maps, before anything else can strip the drop from a `Campaign` |

A row is created **only** when the evidence carries an instance ID, or a full
campaign+drop composite; benefit-only or name-only evidence can enrich an
existing row but can never create one (this is what keeps both of the
schema's unique indexes total).

**Write path — `Observe`**: one transaction per observation, called only
*after* the network call (claim mutation or inventory read) that produced it
has already returned, so no network call ever happens inside a DB
transaction. It looks up an existing row first by exact instance ID, then —
only on a miss — by the exact campaign+drop+occurrence-window composite,
restricted to rows that are either instance-less or already carry the *same*
instance (a row bearing a *different* non-empty instance is invisible to this
lookup, so a second minted instance can never merge into, enrich, or
overwrite the first instance's row — this is what makes two distinct grants
of the same reward two distinct rows). On a miss it inserts a fresh row
(`state = active`); on a hit it *enriches only* — every column upgrades via a
`CASE` guard that fills an empty/unknown value but never overwrites a
populated one, and `evidence_rank` only ever increases. An authoritative
observation for the row's *exact* instance, or the adoption of an
instance-less composite row by a real instance, re-arms the row to `active`
even from `released`/`conflicting` — but this predicate is evaluated against
the row's state *before* the same update, so it can never be triggered by a
*different* instance.

**Self-heal — `Reconcile`**: runs once per full sync (immediately after the
catalog is recorded, on the raw unfiltered candidate set — read-only over
campaigns, writes only to the ledger), applying the first applicable rule per
matched `active` row. The whole candidate set is applied in ONE transaction
(so a timeout/cancellation rolls back the entire pass atomically rather than
half-applying it — simply retried in full on the next sync), and `ctx` is
honored throughout: every skip-ledger DB call (`Observe`/`Reconcile`/
`Snapshot`) is bounded by `DropsTracker.skipLedgerCtx`, derived from the
tracker's own lifecycle context with a capped timeout
(`skipLedgerOpTimeout`), so a cancelled/shutting-down miner — or a slow DB —
can never block behind the process's single SQLite connection indefinitely.

| Rule | Condition | Transition |
|---|---|---|
| SH1 | candidate has an instance ID; the row's instance ID is different (non-empty) | `active → released` (`new_minted_instance`) |
| SH2 | both windows are decidable and disjoint | `active → released` (`disjoint_occurrence`) |
| SH3 | candidate's instance ID equals the row's, and Twitch currently authorizes claiming it | `active → conflicting` (`claimable_same_instance`) |
| SH4 | the row is instance-less, the candidate carries a fresh minted instance with no row of its own yet, and Twitch currently authorizes claiming it | `active → conflicting` (`minted_instance_over_composite_row`) |

A `conflicting`, instance-less composite row separately moves to `released`
(`superseded_by_instance_row`) once *any* instance-bearing row exists for the
same campaign+drop. Time alone never transitions a row — every transition is
driven by a fresh `Observe` or a `Reconcile` pass.

**Read path — `Decide`**: a pure, I/O-free function evaluated once per
candidate drop over a `Snapshot` loaded exactly once per broker pass
(`updateStreamerCampaigns`). A known, *different* game ID on either side
excludes a row from **every** tier below, checked before any of them. In
order: (1) an exact instance match — `active` → **SKIP**, `conflicting` →
FARM, `released` → FARM; a *miss* on the instance still checks whether some
*other* recorded instance (FARM, new occurrence) or an instance-less
claimable composite/benefit row (FARM, stale row) exists before falling
through; (2) a composite (campaign+drop) match with a not-provably-disjoint
window → **SKIP**; (3) a benefit match with a decidable, overlapping window →
**SKIP**; (4) otherwise → FARM. `reward_name` is never consulted anywhere in
this decision.

**Broker-facing filter (S6)**: `updateStreamerCampaigns` loads one `Snapshot`
per pass, off-lock, then builds each campaign's broker-facing view once
(`brokerView`): `Campaign.Clone()` with every `Decide()==SKIP` drop removed. A
nil ledger (never wired) or a failed `Snapshot` load both fail **open** —
`brokerView` returns the original campaign object unchanged, no clone, no
filtering. The tracked pool itself (`Campaigns()`, the drop catalog, every
published `*models.Campaign`) always stays the full, unfiltered set — only
the clone handed to a streamer's `Stream.SetCampaigns` is ever filtered. Its
surviving real unfinished work gates `Streamer.DropsCondition`,
`Streamer.HasEligibleAssignedDropCampaign`, and channel-restricted Drop
authority. The separate channel-side `Stream.CampaignIDs` list remains
advertised/availability evidence only and cannot directly grant a watch slot.

**Diagnostics**: every suppressed drop is logged individually at DEBUG
(`"Drop suppressed by ghost-skip ledger"`, with the campaign/drop identity and
the `decide()` reason — `same_instance`, `same_composite`, etc.), computed
once per campaign per broker pass, matching the pipeline's existing
per-decision logging convention (`logDropIneligible`, `internal/drops/drops.go`
— nothing here rises above DEBUG). The full-sync pipeline's existing
`"Drops sync: campaign counts through the pipeline"` DEBUG summary additionally
carries a `suppressedByGhostSkipLedger` count. And
`DropsTracker.SuppressedDrops()` is a read-only accessor returning the current
list of suppressed drops (campaign/drop identity + reason) for programmatic
diagnostics (e.g. a future dashboard/`/debug/snapshot` view) — like every
other read here, a nil/unwired ledger or a failed snapshot returns nothing
rather than erroring. None of this ever filters `Campaigns()` or any
published campaign; an operator is no longer limited to opening `miner.db` by
hand to see why a campaign stopped being farmed.

**Fail-open guarantee**: a nil ledger, a failed `Observe`/`Reconcile` write,
or a failed `Snapshot` read are all logged and otherwise silently ignored —
none of them can prevent a drop from being claimed or the miner from
starting. `internal/miner/miner.go`'s `setupComponents` wires
`drops.NewSkipLedger` exactly like the pre-existing drop-campaign catalog: a
registration/migration failure is logged and `events.TypeModuleInitFailed` is
recorded, and the miner starts with ghost-skip simply disabled (every
candidate keeps farming, exactly as before this feature existed). Because
`drops.NewSkipLedger` registering its module successfully is not by itself
proof the ledger reached the tracker, `DropsTracker.SkipLedgerEnabled()`
exposes whether `SetSkipLedger` actually ran, so a startup-wiring regression
is provable independently of the module's own schema state.

**Retention**: storage-only, mirroring the drop catalog's own policy, and
**account-scoped exactly like every other statement against this table** —
`Prune(before)` permanently deletes only THIS account's `released` rows past
an operator-supplied horizon; it can never touch another account's ledger
even though every account's rows share the same process-wide `drop_reward_skips`
table. `active`/`conflicting` rows are never pruned, there is no TTL, and no
automatic sweep is wired anywhere — this is an explicit, operator-driven
maintenance action only.

### Channel-Restricted Campaigns

A campaign's `allowedChannels` (parsed from GraphQL `allow.channels`) is either
empty (any channel streaming the game credits progress) or a specific list of
channel IDs (only those channels credit progress).

Per-channel advertised availability comes from a Twitch query
(`DropsHighlightServiceAvailableDrops`, scoped by `channelID`). A returned ID
is not slot authority by itself: `updateStreamerCampaigns`
(`internal/drops/drops.go`) exact-intersects the channel evidence with the
account-known campaign pool and applies the shared Drops evaluator, including
the authoritative `allowedChannels`/`Campaign.AllowsChannel` check. It
publishes only surviving assignments to `Stream.Campaigns`.

Because a channel-restricted campaign can only ever progress by watching that
exact channel, the watcher's `DROPS` priority and rotation boost
(`internal/watcher/watcher.go`) treat streamers holding one as higher
priority only when the assigned restricted campaign still has real unclaimed
work. Such streamers rank above those whose active campaigns are all
unrestricted — an
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
   one goroutine plus locked `State()` reads. Discovery requires a **Known**
   channel-availability snapshot, an exact non-empty advertised-ID/account-known
   campaign intersection, an exact game match, real remaining unclaimed work,
   and an eligible result from the shared Drops evaluator (including ACL). Only
   that survivor set is published to the ephemeral `Stream.Campaigns`.
   UNKNOWN retained IDs, completed/claimed campaigns, malformed or missing game
   identity, and IDs absent from the account pool produce no proposal or
   restricted fact. At most 3 candidates are
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

The optional `discoveryMode` enum (config key, also a "Discovery scope" dropdown
in the Directory Discovery settings panel; `"all"` or `"tracked_only"`, default
`"all"`) selects which channels discovery is allowed to farm — a *candidacy*
decision, orthogonal to the *arbitration* decision `discoveryPreferTracked` makes
below. In `"all"` mode the exclusion gates in `internal/discovery` (`syncOnce`,
`selectBest`, `invalidReason`) skip channels already on the configured streamer
list, so discovery only proposes non-tracked directory channels (the original
behavior). `"tracked_only"` **inverts** those gates: the candidate pool keeps
*only* configured-list channels, and an extra `SlotStatus.IsWatching` gate skips
(and `WatchingOrigin`-based check yields) any tracked channel the rotation is
already watching, so discovery never duplicates the watch minutes of an
already-watched channel — it fills an idle slot with a tracked channel carrying
an active drop that the rotation isn't covering. `config.ValidateConfig`
canonicalizes the value (empty/unknown → `"all"`, the behavior-preserving
default, mirroring `campaignPolicy`), and the mode flows config → DTO →
`Build*`/`ApplyToConfig` → `Miner.ApplySettings` → `discovery.UpdateSettings`,
so it applies at runtime without a restart.

The optional `discoveryPreferTracked` flag (config key, also a checkbox in the
Directory Discovery settings panel; default `false`) narrows this competition:
when set, a discovery candidate may fill an idle slot but may **never** displace
a configured streamer that already holds one (`pickDisplaceable` returns "no
victim" for any non-configured incoming candidate via
`MinuteWatcher.SetPreferConfiguredOverDiscovery`). With the default `false`, the
pre-existing rank-based arbitration stands: a discovered channel farming an
active drop (rank `active_drop`) can displace a configured streamer held only by
points/fair-rotation priority (rank below `active_drop`). Either way, and
regardless of the flag, a channel-restricted discovery drop keeps its normal
rank because discovery derives the restricted fact only from the verified,
eligible unfinished assignment survivor set carried in the same candidate
snapshot the broker arbitrates.

The optional `discoveryPreferSubscribed` flag (config key, also a checkbox in the
Directory Discovery settings panel; default `false`) adds a *tertiary* key to the
candidate comparator in `internal/discovery`, layered over the existing
viewer-count sort: with it on, a subscribed channel floats above a non-subscribed
one within a game, so an otherwise equal per-game pick prefers a subscribed
channel. Game-level policy ranks only pre-order directory fetches and bounded
online checks. Final `selectBest` ordering uses each verified channel's exact
advertised campaign IDs, active tracker intersection, restricted ACL and bounded
Campaign Policy utility: primary `SemanticClass`, then at most one best
qualifying distinct secondary campaign. Configured game order and the existing
stable subscription/viewer order break full bounded-utility ties for a genuinely
new choice. A valid current yields only to a strictly stronger hard/semantic
candidate, so equal facts do not churn. Subscription is a **proxy**: discovery has
no subscriptions GraphQL operation (no such persisted query exists in the
canonical trackers), so instead a slow miner-side loop (`subscriptionProbeLoop`,
base cadence 3 min ±20% jitter, deliberately separate from the 1-minute
`healthWatchdogLoop`) probes a bounded, rotating slice of the pool — at most
`maxCandidateChecksPerTick`+1 channels per tick — with the existing verified
`ChannelPointsContext` operation and treats an active points multiplier
(`ViewerHasPointsMultiplier`, the same signal the `SUBSCRIBED` watch priority
uses) as "subscribed". Results accumulate across ticks in `Manager.subKnown`,
are pruned to the live pool, and are published as a lock-free
`atomic.Pointer[map[string]bool]` snapshot (`SetSubscribedLogins`, mirroring
`SetGameRanks`) that `syncOnce` reads to tag each candidate's `Subscribed` flag.
The probe uses throwaway streamer objects so `LoadChannelPointsContext`'s
unlocked `ActiveMultipliers` write never races the broker loop's use of the pool
streamers, and `RefreshSubscribedSet` clears the set and skips all probing while
the toggle is off (zero cost by default). It flows through the same runtime path
as `discoveryMode`/`discoveryPreferTracked` (config → DTO → `Build*`/
`ApplyToConfig` → `Miner.ApplySettings` → `discovery.UpdateSettings`).

---

## Health Signals (`internal/health`)

The Health Center aggregates the miner's operational signals for the dashboard
(`/health`) and the debug snapshot (`/debug/snapshot`, `health` section). Each
signal records only `status` (`ok`/`degraded`/`failed`/`idle`/`stalled`/
`unknown`), `checkedAt`,
`duration`, `stage`, a short human `detail`, and a stable `errorCode` —
**never** an OAuth token, cookies, a signed playback/spade URL (which embeds
`sig`/`token`), or an authorization header.

The signals are distinct kinds of health:

- **OAuth** — whether the account authorization is still valid (from the miner's
  reauth-required state).
- **GQL API** — whether Twitch GraphQL calls are succeeding (from the API
  client's last-success timestamp vs `connectionTimeoutMinutes`). Reports
  `degraded` when repeated request failures (≥2 exhausted retry cycles within
  the window) accumulate short of a full blackout.
- **PubSub** — evaluated **per connection** rather than on the pool-wide last
  PONG (which would let healthy siblings mask a single broken index): an open,
  non-reconnecting connection whose last PONG is older than the staleness
  threshold reports `failed` (dead socket, `connection_stale`); a connection
  carrying zero topics while the pool holds more than one reports `stalled`
  (lost subscriptions, `topics_lost` — invisible to any PONG-based check); and
  ≥2 reconnects within the window report `degraded` (flapping). With no
  connections yet it falls back to the pool-wide staleness view.
- **Watch Transport** — whether Twitch *accepts the watch transport and beacon*
  (from the canary, below). This is independent of whether any drop is active.
- **Drops Inventory Sync** — whether the periodic inventory sync is running
  without error (from the drops tracker's sync status). A sync that completed
  without error but observed the explicit-null (UNKNOWN) dashboard listing is
  `degraded` with the stable code `dashboard_listing_unavailable` — never
  ordinary "successful discovery" — while an actual sync error keeps its
  `failed`/`sync_error` precedence (see "Dashboard-listing authority").
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
`canCompleteAll` (the whole remaining chain, not just the next reward — both
against `timeUntilEnd − safetyReserve`), `deadlineKnown`, and a status:
`UNKNOWN` when Twitch supplied no real deadline, `SAFE` (finishes the goal
below with margin), `AT_RISK` (finishes that goal but the margin is thin),
`NEXT_REWARD_ONLY` (only the next reward is reachable), or `IMPOSSIBLE` (not
even the next reward, or already ended). Within the same `HighPriority` class,
ENDING_SOONEST orders known real deadlines before `UNKNOWN`; an absent deadline
never becomes the earliest there. The `NextRewardOnly` rule reduces the goal
the *status* is judged against to just the next reward, so a campaign whose
chain no longer fits is no longer downgraded to `NEXT_REWARD_ONLY` once that
reward is reachable — it reads `SAFE`, or `AT_RISK` when the margin over that
reward is thin. It is a goal selection only: `canCompleteNextReward` and
`canCompleteAll` are two independent facts derived from the same snapshot,
and neither ever changes with the rule — `canCompleteAll` keeps reporting
the entire remaining chain.
The rule is not a stop condition: it never excludes a campaign and never
shrinks the remaining work the engine reports.

### Modes

`GAME_ORDER` (default, preserves configured-game ordering), `ENDING_SOONEST`,
`CLOSEST_TO_REWARD`, `LOW_AVAILABILITY`
(fewest eligible live channels first), and `SMART`. `Normalize` upper-cases and
falls back to `GAME_ORDER`; `ValidateConfig` canonicalizes the persisted value
via the same validator (single source of the valid-mode set).

### SMART scoring (itemized)

A weighted sum of named factors, each rendered as a breakdown line: high
priority (+200), channel-restricted (+100), ends within 6h (+80), reward
closeness (tiered +60/+40/+20), sole eligible channel (+30), already-started
(+40), already-in-a-slot stickiness (+10), unstable channel (up to −50), and a
−40 penalty when the selected goal cannot be met (`NEXT_REWARD_ONLY`) — which
the `NextRewardOnly` rule therefore suppresses. Ranking ties break on
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
publishes one immutable watcher snapshot (`SetCampaignSemanticPolicy`, read
lock-free) containing each configured channel's bounded semantic utility, the
exact per-campaign semantic/feasibility facts, and the game-level directory
pre-order. A channel's existing best ordinal `SemanticClass` is always primary.
Only when primary classes are equal may the best one additional distinct
campaign contribute a secondary class. That secondary campaign must have a
non-empty distinct campaign ID, be eligible on that exact channel, have a
positive `minutesToNextReward`, and be `SAFE`, `AT_RISK`, or
`NEXT_REWARD_ONLY`; `UNKNOWN`, `IMPOSSIBLE`, Skip, completed campaigns,
duplicate IDs, and multiple reward tiers of one campaign contribute no
secondary utility. The tuple is compared lexicographically — campaign counts
and classes are never summed — so 2, 5, or 20 weak campaigns cannot overpower
a strictly stronger primary or accumulate more utility than the single best
secondary. The
discovery mirror (`SetCampaignPolicy`) remains an atomic compatibility/fallback
seam; production discovery reads the broker-active snapshot through
`DiscoveryCampaignPolicy`, so a concurrent refresh cannot mix policy
generations between source selection and final arbitration. The watcher's hard eligibility and
restricted/streak classes remain outermost; among otherwise comparable drop
contenders, bounded semantic utility is applied before persisted watch-time
deficit, and deficit remains the fairness authority for a full primary+secondary
tie. Discovery
verifies each candidate's Known advertised campaign IDs against the
account-known pool, exact game, shared Drops evaluator, real remaining work,
and channel ACL. It orders by that channel's bounded exact utility (never the
game's aggregate best), and
carries the exact eligible campaign IDs, their current real-remaining-work
subset, and the restricted fact into the same broker tick; the broker intersects
that fail-closed evidence with the tick's immutable policy snapshot before
resolving utility. It
re-evaluates a still-valid current on each proposal, switching only for a
strictly stronger hard/semantic candidate; equal facts preserve continuity.
No raw policy points, campaign counts, or semantic classes are added to watch
minutes. Without published semantic
facts, configured order and the pre-policy broker behavior are preserved.
Config (`campaignPolicy`, `dropRules`) is
runtime-updatable via the Drops page; the ranked decisions surface on the Drops
page (feasibility badge + breakdown + per-drop controls) and in
`/debug/snapshot` (`policy` section — mode, scores, factors; no secrets).

---

## Chat Integration

### IRC Protocol

| Setting | Value |
|---------|-------|
| Server | `irc.chat.twitch.tv` |
| Port | `6697` (TLS) — the OAuth token is sent as `PASS`, so the plaintext port `6667` is never used |
| Auth | `PASS oauth:{token}` |

#### Connection Sequence
```
1. Connect to server over TLS (crypto/tls, MinVersion TLS 1.2)
2. CAP REQ :twitch.tv/tags twitch.tv/commands  (if chat logging enabled)
3. PASS oauth:{token}
4. NICK {username}
5. JOIN #{channel}
```

On an unexpected disconnect or a Twitch `RECONNECT` command the client
re-establishes the connection automatically with exponential backoff (1s → 30s,
±20% jitter) and replays the sequence above.

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

#### Transactional Migrations & Ownership

Each migration's body and its `schema_versions` bump are applied in **one
transaction** (`applyMigration`): a crash or failure rolls back everything,
so a migration can never end up applied with a stale version, or (for
multi-statement bodies) applied halfway. SQLite DDL is transactional; the
modernc driver executes multi-statement bodies on the transaction's
connection. A `Migration` may define `Run func(*sql.Tx) error` instead of
`SQL` for bodies needing per-statement guards: the two historical
`ALTER TABLE ADD COLUMN` migrations (analytics v3, notifications v2) use
`database.AddColumnIfMissing`, checked per column against
`pragma_table_info`, so databases poisoned by the pre-transactional crash
window (columns present, version stale — previously a fatal
"duplicate column name" loop on every start) self-heal on the next start.

Lifecycle: `database.Open` is a process-wide singleton guarded by a mutex
(not `sync.Once`) — a failed initialization returns the error and is
retryable instead of poisoning later calls with `(nil, nil)`. Ownership is
single: `cmd/miner` opens the DB (always, regardless of `enableAnalytics`),
injects it into the miner via `SetDatabase`, and its deferred `Close` runs
after `Run`/`stop()` return; the miner opens/closes only in library use
(`ownsDB`). `watcher.Stop`/`DropsTracker.Stop` join their loops (bounded by
a 5s `stopJoinTimeout`) so in-flight `watch_time`/catalog writes drain
before the close; the remaining writer joins (pubsub/chat/web) are Stage E
scope. Failures initializing DB-backed modules (notifications, watch-time,
drop catalog) are recorded as `module_init_failed` events on every start in
addition to the error log.

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
-- Durable catalog of every campaign the miner has actually observed in the
-- current/in-progress pipeline, so the "Past" tab can show it after expiry.
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
point is long memory, and it grows only one row per campaign instance. Future
dashboard entries are not recorded; only campaigns observed in the current or
inventory-in-progress pipeline populate this durable history.

**Note**: All timestamps are Unix timestamps in milliseconds, except `watch_time_events.timestamp` which is Unix seconds.

#### Drop Skip Ledger Module Schema

```sql
-- Durable, evidence-ranked record of drop rewards this account has been
-- authoritatively granted (or Twitch's inventory reports as already claimed),
-- scoped per account_key (config.StorageKey()). See "Drop Skip Ledger" above
-- for the evidence classes, write/self-heal/read paths, and the fail-open
-- contract; this table is consulted ONLY to gate future broker-facing drop
-- assignment, never Drop.CanClaim or the claim mutation itself.
CREATE TABLE drop_reward_skips (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account_key    TEXT    NOT NULL,
    game_id        TEXT    NOT NULL DEFAULT '',
    instance_id    TEXT    NOT NULL DEFAULT '',
    benefit_id     TEXT    NOT NULL DEFAULT '',
    campaign_id    TEXT    NOT NULL DEFAULT '',
    drop_id        TEXT    NOT NULL DEFAULT '',
    reward_name    TEXT    NOT NULL DEFAULT '',   -- diagnostics only, never matched
    occ_start_ms   INTEGER NOT NULL DEFAULT 0,    -- entitlement-window bounds (Unix millis, 0 = unknown)
    occ_end_ms     INTEGER NOT NULL DEFAULT 0,
    occ_source     INTEGER NOT NULL DEFAULT 0,    -- models.WindowSource
    occ_known      INTEGER NOT NULL DEFAULT 0,    -- 1 iff the window is authoritative (models.EntitlementWindow.Known)
    evidence_class TEXT    NOT NULL,              -- claim_accepted | claim_already | inventory_claimed
    evidence_rank  INTEGER NOT NULL,               -- 3 | 2 | 1, monotone non-decreasing on a row
    state          TEXT    NOT NULL DEFAULT 'active',  -- active | released | conflicting
    state_reason   TEXT    NOT NULL DEFAULT '',
    created_at_ms  INTEGER NOT NULL,               -- never updated after insert
    updated_at_ms  INTEGER NOT NULL,
    resolved_at_ms INTEGER NOT NULL DEFAULT 0      -- stamped only on a transition INTO 'released'
);

-- instance_id is part of the composite tuple below (not just its own index):
-- two server-minted instances of the same campaign+drop+occurrence are two
-- distinct grants and must never collapse onto one row.
CREATE UNIQUE INDEX ux_drop_skips_instance
    ON drop_reward_skips(account_key, instance_id) WHERE instance_id <> '';

CREATE UNIQUE INDEX ux_drop_skips_composite
    ON drop_reward_skips(account_key, campaign_id, drop_id, occ_start_ms, occ_end_ms, instance_id)
    WHERE campaign_id <> '' AND drop_id <> '';

CREATE INDEX idx_drop_skips_benefit ON drop_reward_skips(account_key, benefit_id) WHERE benefit_id <> '';
CREATE INDEX idx_drop_skips_composite_lookup ON drop_reward_skips(account_key, campaign_id, drop_id);
CREATE INDEX idx_drop_skips_state ON drop_reward_skips(account_key, state);
```

A row is created only when the evidence carries an instance ID, or a full
campaign+drop composite (benefit-only/name-only evidence can enrich an
existing row but never creates one — see "Drop Skip Ledger" above), which is
what keeps both unique indexes total over every row this package ever writes.
Like the drop catalog, this table is excluded from any automatic retention
sweep; `SkipLedger.Prune` (never called automatically) deletes only that
ledger's own `account_key`'s `released` rows past an operator-supplied
horizon — every account's ledger shares this one process-wide table, so the
`account_key` predicate on the `DELETE` is what keeps one account's `Prune`
call from ever touching another's rows.

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

1. **Emit** — When a tracked, confirmed prediction resolves — i.e. the
   delivery that wins the terminal admission (see *Terminal Result Admission
   (tracked-only)*) — `pubsub.WebSocketPool`
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
| `WIN` | Green (#36b535) | Prediction won (tracked rounds only — see *Terminal Result Admission (tracked-only)*) |
| `LOSE` | Red (#ff4545) | Prediction lost (tracked rounds only — see *Terminal Result Admission (tracked-only)*) |

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
| `/api/lifecycle` | GET | Current lifecycle snapshot (desired/observed/transition, capabilities, override, update state) as JSON, or the `lifecycle_panel` HTMX partial; never gated (read-only) |
| `/api/lifecycle/{action}` | POST | Lifecycle mutation: `pause`/`resume`/`restart`/`stop`/`restart-process` (action taken from the URL suffix); gated per [Dashboard Security Model](#dashboard-security-model-internalwebsecuritygo) — refused under `DASHBOARD_INSECURE_NO_AUTH=true` unless the caller's own remote address is in `DASHBOARD_TRUSTED_LAN_CIDRS` |

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
| `colored` | bool | false | Enable colored output |
| `autoClear` | bool | true | Write-triggered ~24h segmented rotation; retain at most 7 completed segments |
| `timeZone` | string | null | Custom timezone |

With `save=true`, the canonical active file is `logs/<StorageKey>.log`. When
`autoClear=true`, the first write after approximately 24 hours of segment age
renames the old active file to
`<active>.rotated-<20-digit-monotonic-sequence>` and writes the triggering
record to a new canonical active file. At most seven completed owned segments
are retained. With `autoClear=false`, logging remains ordinary append-only and
does not rotate or prune. The dashboard reads the newest 500 complete lines
under one aggregate 2 MiB budget; `/debug/log` defaults to 1000 lines, clamps
at 2000, and uses one aggregate 4 MiB budget. Both readers traverse the retained
family and return records in chronological order.

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
├── hasPreconditionsMet: bool | null (null is distinct from false and true)
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
3. Wait `rateLimits.reconnectDelay` seconds (configurable 30-300, default 60)
4. Create new connection
5. Re-subscribe to all topics

**HTTP Requests (generic GQL reads/non-bonus operations):**
1. Catch a transient failure (connection error, rate limiting, 5xx)
2. Retry with exponential backoff plus random jitter
3. Give up after `gqlMaxRetries` = 4 retries (up to 5 attempts total per
   client ID) and surface the error to the caller

`ClaimCommunityPoints` is the non-idempotent exception and follows the bounded,
fail-closed policy in *Bonus claim arbitration*; it never uses this generic
transient retry loop.

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
│   └── {username}.json       # Authentication tokens (JSON; optionally AES-256-GCM encrypted)
├── logs/
│   ├── {StorageKey}.log      # Canonical active plain slog text
│   └── {StorageKey}.log.rotated-{20-digit-sequence}
│                             # Up to 7 completed owned segments when autoClear=true
└── database/
    └── {StorageKey}/
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
owner-only `0600` permissions. The temp file is fsynced before the rename;
the containing directory is fsynced **best-effort** after it (errors ignored —
meaningful on Unix, a no-op elsewhere), so durability of the rename across a
power loss is best-effort rather than strictly guaranteed. `LoadConfig` migrates a pre-hardening `0644`
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

Stable is a closed, independent channel. Its selector exhausts the paginated
GitHub Releases collection and accepts only canonical
`stable-vMAJOR.MINOR.PATCH` public, non-prerelease Releases with the exact
Linux amd64/arm64 binary set plus `checksums.txt`. It maps that tag explicitly
to public `MAJOR.MINOR.PATCH`; generic/main `v*` values never enter candidate
ordering. Each stable download must match both the strict checksum file and
GitHub's server-side `sha256:` asset digest, and must contain exactly one
platform-bound `BTM_STABLE_ARTIFACT_V1` marker with the same `VERSION` and
`CHANNEL=stable`. Stable build initialization derives its runtime Version and
Channel from that single marker.

Before cache or swap, stable resolves the exact Git tag (including bounded
annotated-tag indirection) to a commit and queries GitHub artifact attestations
by the downloaded sha256. Verification uses Sigstore's TUF-distributed
public-good trust root and requires the Fulcio certificate signature, SCT,
Rekor transparency inclusion/observer timestamp, exact GitHub Actions OIDC
issuer, public hosted-runner build, this repository, the exact stable workflow
at the exact tag ref, and the resolved commit in the source/build signer
extensions. The verified in-toto statement must be SLSA provenance v1 and must
bind exactly both controlled binary names/digests, the same workflow inputs,
builder identity, tag ref, and Git dependency commit. Absence, ambiguity, trust
root failure, signature failure, or any claim mismatch refuses apply.

Before the live swap, the stable updater must atomically activate the verified
candidate in a bounded two-slot cache at
`database/.updater/stable/<goos>-<goarch>`. Cache failure blocks apply. At the
cache boundary, a candidate below the already accepted version is refused and
the same public version may never change digest/source identity; a failed live
swap therefore cannot expose the durable floor to a later regression. At the
next process start, before healthcheck/config/application work, stable-only
bootstrap validates the strict manifest, binary hash, tag/version/channel,
platform, asset name, API digest binding, verified source-commit shape, and
embedded marker. The cache contains only a candidate that already passed the
live Sigstore/SLSA gate; TUF metadata is kept in a sibling persistent cache. A
cached version newer than the pinned executable is restored atomically and
re-executed; a same/older version is ignored, so ordinary container recreation
cannot cause a silent downgrade. `AUTO_UPDATE=false` disables future
acceptance but not replay of a previously accepted floor. This bootstrap is
deterministic recovery, not a second discovery/updater state machine.

The stable producer keeps the immutable GHCR image, SBOM, and OCI provenance,
and additionally builds the exact two raw updater binaries, checksums, and a
GitHub build-provenance attestation. It uploads them without overwrite only to
an already existing exact-tag public Release after the image succeeds. The
scratch runtime verifies that cryptographic attestation in-process as described
above; the producer's OCI image attestation remains independently auditable.

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

**Trusted-LAN lifecycle allowlist.** Under `DASHBOARD_INSECURE_NO_AUTH=true`
— and *only* then; this has no effect at all when Basic Auth is configured,
nor on a loopback-default run with no auth mode set — every lifecycle
mutation POST (`pause`/`resume`/`restart`/`stop`/`restart-process`)
additionally passes through `lifecycleLANTrust`, a tri-state classifier over
the `DASHBOARD_TRUSTED_LAN_CIDRS` allowlist
(`internal/runtimeconfig.ParseTrustedLANCIDRs` → `[]netip.Prefix`):
`notConfigured` (no allowlist set → today's unconditional 403, outcome kind
`insecure`, unchanged), `allowed` (the request's `r.RemoteAddr` falls inside
a configured CIDR → the mutation proceeds normally), or `denied` (an
allowlist is configured but the remote address is outside every prefix, or
the address itself failed to parse → 403, **new** outcome kind
`lan_denied`, the lifecycle controller is never invoked). The classifier
trusts ONLY the TCP connection's own peer address — never `Forwarded`/
`X-Forwarded-For`/`X-Real-IP`, all of which an untrusted client can set to
any value — so it does **not** work behind a reverse proxy unless the
proxy's own address is what is allowlisted. Each entry must already be its
own canonical network address (`netip.Prefix` equal to its own `.Masked()`
— a set host bit, e.g. `192.168.1.5/24`, is rejected rather than silently
normalized) and must not be an IPv4-mapped-IPv6 form (`Is4In6`, e.g.
`::ffff:192.168.0.0/112`, which could never match since the connection
address is always `Unmap()`ed first). Parsing fails closed: an invalid
`DASHBOARD_TRUSTED_LAN_CIDRS` entry is captured as
`Dashboard.TrustedLANCIDRsErr` at bootstrap and re-checked FIRST in
`validateBindSecurity` — before the loopback short-circuit — so the process
refuses to start with an actionable message naming the variable, regardless
of bind host or auth mode (whenever the dashboard is enabled at all — with
`EnableAnalytics=false` no web server is built, and no lifecycle surface
exists). Basic Auth, when configured, is never bypassed by
this allowlist (its own check happens earlier in the middleware chain via
`basicAuthMiddleware`, and this classifier is only ever consulted once
`DASHBOARD_INSECURE_NO_AUTH=true` is already established), and
`csrfProtectMiddleware`'s same-origin check still runs before this gate is
ever reached — the allowlist widens who may skip *authentication*, never who
may skip CSRF. `GET /api/lifecycle` is never gated by any of this, in any
trust state.

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
