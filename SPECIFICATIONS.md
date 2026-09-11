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
│   ├── prediction_observation.go # Immutable Prediction observation trail (v6)
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
| `RewardList` | `0b1471876d7647993731b9e3c6a13bf304c67fb31d07f06a945d42286ee377c4` | **Diagnostic only.** Read-only Watch Streak milestone observation (`channelID`, `shouldIncludeAllSuspendedStreaks: false`). Grants nothing and controls nothing — see *Watch Streak milestone observability*. Hash is captured protocol evidence; live acceptance is **UNKNOWN/PENDING** runtime evidence, and a `PersistedQueryNotFound` outcome is the expected honest result, never grounds to invent a replacement hash |

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
  the promoted default, then the rest. That order is for BUSINESS reads: a
  diagnostic read caches nothing per operation and therefore starts from the
  current process-wide default — the shipped one until a BUSINESS rotation
  promotes another — followed by the remaining shipped candidates, and it
  never moves that default itself. A client ID that resolves a
  `PersistedQueryNotFound` is cached for the operation and promoted to the global
  default, logged **once** on the actual promotion (no per-request spam, even
  under concurrent recovery). The active default is surfaced in the Health Center
  as the *Active GQL Client ID*.
- **Distinct error, not "streamer does not exist".** When *every* candidate
  client ID still returns `PersistedQueryNotFound`, the hash itself is stale:
  `postGQLRequest` returns `twitch.ErrPersistedQueryNotFound` (for a **business** read, one `ERROR` log naming
  the operation and the count of client IDs tried; a **diagnostic** read logs the same
  summary at `DEBUG` — see *Watch Streak milestone observability*). Callers treat this as a
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
| `community-points-user-v1` | `points-earned` | Update balance, log earnings, persist the accepted event to the exact point-event ledger (see *Exact Point-Event Ledger*); the frame's `point_gain` carries `total_points` (the accounted amount), `baseline_points` (read only by the Watch Streak grant-correlation label, for its ladder basis; see *Watch Streak milestone observability*) and `multipliers` (not read by any code; its presence beside `baseline_points` in the fixtures is the evidence that label's pre-multiplier assumption rests on) |
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

- **Persisted fairness on the broker tick:** every ordinary broker evaluation ranks each online streamer by accumulated watch minutes over the trailing 8-hour window, persisted in SQLite (`watch_time_events`, module `watch_time`, survives container restarts), and gives the base slots to the two with the *least* accumulated time. There is no randomized dwell and no rotation-interval setting. Ties (including cold start) use in-memory recency and then normalized login, so candidate permutation cannot change the result. Whoever is watched accumulates minutes and becomes less owed, surfacing every valid contender without an in-memory cursor or parity special case.
- **Minimum residence of the committed ordinary cohort:** the ordinary watch-slot service that was actually GRANTED keeps its seats for a minimum of 15 minutes (`internal/watcher.fairRotationResidence`) before an *ordinary* persisted-deficit challenger may take one. The protected object is the **committed** cohort, not the Phase-A pair: the pair is a proposal, and a proposal that never held both its seats — because a DROPS/STREAK boost or a cross-source displacement took one — cannot be what residence protects. `rotationState.committedCohort` (login-keyed, with the broadcast identity each seat's term was granted on), its single common anchor `cohortSince`, and the residual ordinary capacity `cohortCapacity` are that one owner; they are process-local loop-owned state, with no goroutine, timer, store, ledger, cache or setting behind them, and the loop derives elapsed residence from them on the broker tick it already runs. `rotationState.lastSwitch` is **not** an anchor: it keeps only its documented meaning, "when `activePair` last actually changed", which is what diagnostics publish as `PairSince`. The diagnostic slot journal's `slotResidence` is unrelated bookkeeping with no scheduling authority. One evaluation reads one instant: `processWatching` captures `now` once and Phase-A selection, Phase-B arbitration and the commit all measure residence against it, so a deadline cannot fall between two decisions in the same tick. A consequence worth stating: because `PairSince` is the base-pair timestamp and the residence anchor is not published, `PairSince` alone no longer explains why an ordinary seat has not moved — exposing the cohort anchor would be a new public diagnostic surface and is deliberately not added here.
  - **Role partition (S/O) and residual capacity C.** A committed slot is *ordinary* when it was obtained by the ordinary selection role — persisted-deficit fair rotation, or the direct-mode priority pick — and *stronger* when a stronger admission took it: an external/discovery proposal (including a proven provisional one), or a configured channel the DROPS/STREAK overlay seated OFF the base pair. A boost target that persisted fairness itself brought INTO the base pair is ordinary, because fairness is why it holds the seat. `C = MaxSimultaneousStreams - |S|` counts the seats a stronger admission *actually took*; it is **not** two minus every high-labelled candidate, so a candidate that was not admitted reserves nothing and channels carrying equal drop semantics keep competing for an ordinary seat by persisted deficit. A direct pair of equal-semantic drop channels is `C=2`, not `C=0`.
  - **Capacity states and transitions.** `C=2` protects the valid unordered ordinary pair and `C=1` protects the actual singleton against ordinary fairness replacement. Residence is invalidated whenever the committed ordinary cohort is EMPTY — which is what "C=0" means here, and is not the same as the arithmetic `C` reaching zero: a tick that commits one stronger seat and no ordinary one leaves `C=1` and still invalidates, because there is no ordinary service to protect. Nothing is promised while the cohort is empty. A real change of committed MEMBERSHIP or capacity starts one fresh **common** anchor: `2→1` shrinks immediately, `1→2` refills immediately, `→0` invalidates, `0→1/2` initializes from its own commit. Broadcast identity is deliberately not in that list; it acts only in the shortening direction, on one member (see below). Permitted unused capacity is never held back: every seat the residents do not occupy is filled from the same fresh ranking on the same evaluation.
  - **What does not restart a term.** A permutation of the roster or of seat order, a cosmetic reason/campaign relabel on a seat the cohort already holds, a session refresh, a rejected proposal, a change among unselected candidates, and a stronger occupant handing off to a *different* stronger occupant at unchanged capacity all leave the anchor exactly where it was. A seat committed before its broadcast identity was known and later observed on a real broadcast has not been replaced either — the same session merely became identifiable. A genuine **replacement** of a known broadcast is a real change, but it can only ever SHORTEN a term, never renew one: the replacing member drops out of residence at once and competes for its seat on persisted deficit like any other candidate, while the common anchor — and therefore every other member's term — is left exactly where it was. The direction is load-bearing, not a detail. One anchor is common to the whole cohort, so re-stamping it on one member's new broadcast would also renew a partner whose service never stopped, and a channel that starts a new broadcast every few minutes could hold both ordinary seats for the whole uptime and keep the rest of the ranking out. Because the recorded identity is what the term was granted on, it is not refreshed while the term stands; only a seat committed before its broadcast was known has its identity filled in, which identifies the session the term already covers rather than replacing it.
  - **Residence never delays a stronger cause.** It is consulted only on ordinary branches — the persisted-deficit ranking, the boost's victim choice, and the cross-source displacement's victim choice — and always *below* hard reason class and campaign semantics. Stronger admission and preemption, an offline/ineligible/avoided/removed channel, an invalid session or proof, and the established recovery paths therefore all take effect at the next eligible completed evaluation, exactly as before. A removed or renamed channel stops being protected at once. Expiry only re-opens ordinary ranking: nothing switches because the deadline passed, and the same cohort is kept whenever it is still the most owed.
  - **Turns follow committed grants.** `rotationState.lastWatched` records the channels that actually RECEIVED a slot, settled with the residence as soon as the allocation is final (immediately after final proof reconciliation, before the beacons are sent), so a proposal the broker rejected, a provisional overlay whose final proof was refused, and a tick cancelled before the commit all consume no turn and no tenure. The generation check and the commit are taken TOGETHER, under the same lock `Stop` cancels under: the allocation is settled inside a section guarded by a different lock, which `Stop` releases before it cancels, so a check read merely beside the commit would leave a window in which a cancellation lands between the two and the next generation protects seats this one never served. One window remains closed by construction rather than by locking: a caller cancelling the PARENT context passed to `Start` — the signal path — ends the derived generation through the context package and holds no lock of the watcher's, so a tick can still commit an instant after its generation died. Admitting a fresh generation therefore DROPS the previous one's residence, anchor, residual capacity and rotation recency, which makes such a commit inconsequential instead of racing to prevent it. Persisted fairness history is untouched by that: it lives in `WatchTimeStore`, and a new generation reads exactly the same history it always did. One consequence is worth naming: rotation recency is also the LAST tie-break in `betterBoostCandidate`, which picks between off-pair boost candidates the strict class ordering rates exactly equal. Redefining recency therefore can pick a different one of two equally strong candidates — a fairness decision between equals, now made on service actually delivered rather than on who was proposed. It cannot change the ordering itself: a strictly stronger class still wins regardless of recency, and equal campaign semantics still refuse to displace.
  - **Ties among ordinary seats are broken by persisted deficit.** When residence cannot separate two ordinary occupants — both resident, or neither — the victim is the one with MORE accumulated watch time, i.e. the less owed, for plain seats as well as campaign ties. This matters because a real change of committed membership or capacity starts a fresh anchor: a stronger occupant that arrives and leaves on alternating evaluations re-anchors the cohort every time, so the deadline is never reached and this tie-break is the only thing left deciding. Falling through to recency or a fixed order there would hold one channel in a seat for the whole uptime, against the ranking the residence exists to serve. Persisted fairness minutes are still written only by the existing successful-delivery path (a positive `Stream.UpdateMinuteWatched` delta reaching `WatchTimeStore.RecordMinutes`); residence credits nothing.
  - **What is guaranteed, and what is not.** With a fixed eligible set, constant positive residual capacity, bounded weights and preferences, real delivered progress and bounded evaluation delay, every eligible ordinary channel receives a residence turn: at `C=1` with three equal-history channels and successful delivery each minute, the third channel's first grant lands by 30 minutes and all three have been served by 45; at `C=2` the third is served at the first evaluation at or past the minimum residence (the deadline comparison is strict, so exactly `R` elapsed already re-opens the ranking). No universal production no-starvation claim follows from this, and none is made. A stronger occupant that arrives and leaves on alternating evaluations re-anchors the cohort each time, so the residence deadline is never reached — that is the "bounded interruptions" precondition above doing exactly what it says. Under that churn the persisted-deficit tie-break above is what keeps service even, and it does: with equal history and equal delivery no channel takes more than double another's share. A channel whose persisted credit is suppressed (a tombstoned login) stays permanently most-owed, and a committed seat whose beacon is suppressed by an existing provisional-lease conflict still owns its turn — both are pre-existing behaviours this rule inherits rather than introduces. Residence promises no per-channel 15 minutes under all circumstances, and it is not a Twitch-earnings guarantee.
  - **Preference latency is unchanged.** Residence applies to the ordinary ranking's own inputs (persisted deficit, the prefer handicap, and the recency/login tie-break), so `avoid` removes a channel from the candidate set and takes effect on the next tick, while `prefer` only re-ranks and so takes effect at the next ordinary reconciliation.

- **Priority as a boost, not exclusivity:** on top of the weighted base pair, an online streamer with an active drop (`DROPS`) or a pursuit-eligible watch streak (`STREAK`) can take over one seat without altering persisted weights. Watch Streak continuity across a base-pair reconciliation covers the zero-minute `ELIGIBLE` bootstrap needed to bank the first delivered interval and the genuinely `PURSUING` state; it ends on a bound authoritative grant, the exact 20-minute `TIMED_OUT_UNKNOWN` transition, lost eligibility, or a strictly stronger hard/semantic contender. An ordinary active or channel-restricted drop has no equal-class continuity exception: strictly stronger current facts still win, while a full hard/semantic tie converges to persisted-deficit fairness instead of preserving a previous latch. Hard restricted/streak/drop class is compared first. Comparable drop contenders then compare Campaign Policy utility lexicographically: primary `SemanticClass`, presence of at most one qualifying distinct secondary campaign, then that secondary's `SemanticClass`. Recency and normalized login finish deterministic ties without granting a third slot.
- **Continuous-watch accounting:** `Stream.MinuteWatched` measures *continuous successfully delivered* watched minutes for one exact non-empty `BroadcastID`, not broadcast age, uptime, discovery age, scheduler ticks, failed reports, or wall-clock dwell. A real slot loss resets only the continuous counter and its report anchor. It preserves the grant ledger, broadcast binding, and same-broadcast timeout latch, so reacquiring a timed-out broadcast never opens a second 20-minute window. A transient status blip with the same `BroadcastID` is not a new broadcast; only a genuinely changed non-empty `BroadcastID` re-arms a broadcast-specific pursuit.
- **Single watch-streak owner:** `Stream` derives every direct-selection and broker-protection verdict from the same state: `ELIGIBLE → PURSUING → GRANTED | TIMED_OUT_UNKNOWN`. Zero, 7, 8, 15, and 19 delivered minutes remain eligible/pursuing; 15 minutes is diagnostic only and causes no transition. Exactly 20 minutes without a proven bound grant latches `TIMED_OUT_UNKNOWN`, releases streak priority, and persists that outcome through the existing atomic streak cache. Timeout means outcome unknown, never failed, missed, impossible, or inactive. A bound authoritative grant dominates timeout; a late grant is accepted once without reopening pursuit.
- **Grant attribution and replay:** ordinary `WATCH` is delivery evidence only and can never grant a streak. An authoritative `WATCH_STREAK` is admitted exactly once by its canonical PubSub event fingerprint. When independent evidence proves a `BroadcastID`, the grant is `GRANTED` for that broadcast; otherwise it is persisted and counted explicitly as `GRANTED_UNBOUND`, never guessed onto the currently observed broadcast and never allowed to end, re-arm, or time out that broadcast's pursuit. Terminal broadcast facts and exact replay identities do not expire by wall-clock age; a proven new `BroadcastID` is the only re-arm signal. WebSocket replay suppression uses the full canonical topic+payload fingerprint, so distinct back-to-back point events are not collapsed.
- **Watch-streak pursuit diagnostics:** the watcher logs pursuit once and the exact 20-minute release once. Release is outcome-neutral (`TIMED_OUT_UNKNOWN`); WATCH-credit count is diagnostic evidence only, and neither the historical 7-minute hint nor the 15-minute diagnostic reference controls eligibility, protection, displacement, or timeout.
- **Watch Streak milestone observability (diagnostic only):** an optional, read-only `RewardList` observation stage runs on the EXISTING bonus poll cycle (`internal/miner.bonusPollLoop`), strictly AFTER that cycle's business pass (bonus claiming and auto-redeem) has returned — never interleaved with it. It owns no ticker, goroutine, timer, cache, ledger, migration, public API, Statistics/UI field or support-bundle contract, and it adds no settings surface. Each online streamer with a channel identity is visited at most **once** per bonus cycle, in roster order from a loop-owned cursor, and the whole cycle spends at most **three** explicit diagnostic HTTP attempts, shared across every target, retry and client-ID fallback — the shared read transport's existing fallback and transient-retry contract still applies, but it draws on that same allowance rather than multiplying it (see the *business-first deadline, roster cursor and cycle allowance* bullet below). The stage runs under a **cycle budget** (`milestoneObservationCycleBudget`, 40s): it is a plain call on the poll goroutine, so an unbounded stage would defer the next *business* bonus pass for as long as a slow Twitch kept the retry schedule running, once per target. The value is shorter than `bonusPollInterval` (60s), so the stage alone can never push the next business pass past its tick, and a single budget is used rather than a per-request one so a slow roster cannot multiply the bound by its length. That budget is one of three bounds: the stage's deadline is the EARLIEST of `stageStart + 40s`, the next business tick (`tick + bonusPollInterval`, computed from the timestamp the serviced ticker event carries) and the owner context's own deadline, so business work that already ran long is never compounded by a diagnostic, and a cycle whose period is already consumed admits no diagnostic request at all. It is also longer than one shared-transport HTTP timeout (30s), but NOT — as an earlier revision of this paragraph claimed — because that margin lets a genuine Twitch stall classify as `TRANSPORT_TIMEOUT` rather than being masked by our own deadline. **Measured, that claim is false.** A `Client.Timeout` error carries status code 0, which the shared retry contract named two sentences above classifies as TRANSIENT, so the first 30s timeout is never returned to this caller — it is retried — up to the cycle allowance of three dispatches, itself below the transport's `gqlMaxRetries`+1 ladder — plus backoff. The budget therefore expires during attempt 2, the error the stage sees is its own context's, and the record reads `CANCELLED`/`DEADLINE_EXCEEDED` (reproduced once by hand against a transport that never answers, with the shipped 30s client timeout and the 40s budget: 40.009s, `DEADLINE_EXCEEDED`; a one-off figure, not pinned by a test). The retry half of that mechanism — a `Client.Timeout` being retried rather than returned on its first occurrence — is pinned by `TestAClientTimeoutIsRetriedRatherThanReturned` in `internal/twitch`, which is the only package able to shorten the client's hard-coded 30s timeout without adding a production seam; the miner-layer test can observe only the consequence. `MilestoneFailureTransportTimeout` is consequently NOT reachable through this stage for a pure pre-response stall, and under the three-dispatch allowance the retry ladder never reaches its final attempt through this stage either, so a pre-response stall on a target that starts with at least two permits ends as the stage's own `DEADLINE_EXCEEDED` (the 30s client timeout plus one backoff put the second dispatch past the 40s budget before a third permit could be charged); a stall on a target that starts with the cycle's LAST permit ends as `ALLOWANCE_EXHAUSTED` after its first 30s timeout, with budget to spare and the owner alive, because the retry loop finds no permit for a second attempt (pinned at test scale by `TestAStallOnTheLastPermitEndsAsAllowanceExhausted` in `internal/twitch`), as does a FAST-failing transient — an immediate 5xx or a refused connection, not a stall — whatever the permit count. What DOES reach `TRANSPORT_TIMEOUT` through the stage is a body stall — a 2xx status received and then a body that never completes — which `Client.Timeout` ends as a NON-transient read error on the first dispatch, with the owner alive (pinned by `TestABodyStallAfterA2xxReadsTransportTimeoutOnTheFirstDispatch` in `internal/twitch`); it also stays reachable for a caller passing a nil allowance with a longer-lived context. Making the pure-stall route reachable here would need a budget exceeding the whole retry schedule, far past `bonusPollInterval`, which defeats the bound — a cadence decision rather than a documentation fix, so the documentation is what changed. A cycle stopped before its roster was finished emits one `DEBUG` `watch_streak_milestone_budget` record stating WHY (`cutoff=TIME` for the deadline, `cutoff=COUNT` for the spent allowance), WHICH bound produced the deadline (`deadlineSource=STAGE_BUDGET|NEXT_TICK|OWNER`, with the slack it left in `slackMs`), how many eligible targets were started (`startedTargets`) and how many eligible targets were left unexamined (`unexaminedTargets` — eligible targets that made no dispatch, which is the set the cursor revisits first, except for a target whose channel identity disappeared after the mask was taken (counted here, skipped without moving the cursor)), and the allowance spent and remaining (`dispatchesSpent`, `dispatchesRemaining`, `dispatchAllowance`), with the stage budget in force beside them (`cycleBudgetSeconds`), because a silently short cycle would read as "those streamers had nothing"; a cycle with no slack emits the same record with nothing started. Each observation record additionally carries `dispatches`, the explicit attempts that observation made against the shared allowance. Cancelling the loop context releases the in-flight request and abandons the remaining roster, and emits no budget record: the process is going away, so a record nobody will read is not evidence. The target in flight still emits its own observation record, with outcome `CANCELLED`. Everything it produces is a structured diagnostic **log record**, and retained logs are the ONLY persistence this feature has. The two record streams sit at different levels on purpose: `watch_streak_milestone_observation` is emitted at **`DEBUG`** for every outcome, because it recurs every cycle for up to three started targets (one per cycle in the unaccepted-hash steady state); `watch_streak_grant_correlation` is emitted at **`INFO`**, because it occurs only on an accepted `WATCH_STREAK` grant and belongs beside the other per-grant points events. `DEBUG` is deliberate: the console handler filters on level alone and defaults to `INFO`, so an `INFO` record would print one line per started target per cycle (up to three) to stdout for as long as the miner runs, and in this feature's expected steady state (an unaccepted `RewardList` hash) those lines carry no observed data at all. `FileLevel` defaults to `DEBUG`, so the observation records still reach the retained log; raising `FileLevel` above `DEBUG` turns the observation evidence off (the `INFO` correlation records survive), which is the honest trade for owning no settings surface. Measured cost, since the feature has no gate — method: one `slog.TextHandler` at `DEBUG` (the handler shape `internal/logger` uses for the file), the default test fixture, pinned with bounds and logged by `TestRecordFootprintIsMeasuredAndBounded` in `internal/miner`: **about 1,127 bytes per `OBSERVED` record** (a fixture-typical size, not a maximum: every observed string is capped at 128 characters and the identifier sample at 8 entries, so a fully populated record can be roughly 2–3× larger), **about 1,633 bytes across exactly 3 lines for an unaccepted-hash cycle** on a two-target roster (the observation record, the transport's all-candidates `DEBUG` summary, and the budget record), and **332 bytes per budget record** — the byte counts vary by one or two between runs with the width of timestamps and sequence numbers, which is why the test pins them with bounds (1,536 / 2,048 / 512 bytes and the 3-line shape) rather than exact values. The allowance admits at most three started targets per cycle whatever the roster size — and in the unaccepted-hash steady state exactly one, because a complete client-ID traversal costs all three attempts — so the per-cycle volume is bounded by the allowance rather than by the roster: at most three observation records plus one budget record per cycle. Log rotation is by TIME, not size: with the default `autoClear=true`, 7 completed 24-hour segments are kept beside the active one, so up to 8 days of records sit on disk (with `autoClear=false` the writer neither rotates nor prunes, so the figures below are an 8-day rate, not a cap). Derived from those measurements over the 10,080 cycles of the 7 completed segments (add one eighth — 11,520 cycles, ≈39 MB / ≈43 MB / ≈19 MB — for a nearly full active segment): about 3.4 KB per cycle and ~34 MB when the hash is accepted (≈37 MB with the budget record on a roster larger than three), and about 1.6 KB per cycle and ~16 MB in the unaccepted-hash steady state, independent of roster size. Earlier revisions quoted ~69 MB for 5 online streamers and ~276 MB for 20 for the uncapped stage; those figures were not derivable from the stated per-record numbers and are withdrawn rather than restated. A diagnostic read deliberately omits the shared transport's per-attempt success trace (`GQL response`) and its per-candidate `PersistedQueryNotFound` `WARN`, which would otherwise roughly double that; the single exhausted-candidates summary is kept at `DEBUG`, and the per-retry line (`GQL request failed, retrying`) is kept at `DEBUG` without its error text — at most two per cycle under the three-permit allowance, and not part of the measured unaccepted-hash cycle, which contains no retry; business reads keep the full trace. Specifically:
  - **Observations are facts, not verdicts.** The parser preserves `MISSING` != `NULL` != `EMPTY` != `VALID` != `MALFORMED` at every node — including the per-element `id` of each nested `broadcastIdentifiers` array, whose classifications reach the record as a presence tally rather than a single aggregate count — and never coerces an unobserved or malformed field into a zero, an empty container or a false. Success is judged on the HTTP status and the response's GraphQL shape, never on the body merely looking right: any status but **HTTP 200** — the only status this contract names as a GraphQL answer; a 202 or a 206 is a pending or partial answer it never describes, and the bonus-mutation path already refuses everything but 200 — is refused as `HTTP_STATUS` on the status alone, without its payload being parsed or trusted (with a 2xx-range gate, a 206 carrying a valid `data` object was recorded `OBSERVED` and a 202 carrying a structured APQ rejection drove the client-ID walk to `UNSUPPORTED_QUERY` after three authenticated dispatches; pinned by `TestOnlyHTTP200IsDiagnosticEvidence`, whose business control shows a business read still decoding a 206) (the body is still received by the shared transport, but a diagnostic read will not pull more than one byte past 1 MiB of it into memory, so a rejected request cannot be answered with an unbounded one; a body past that limit is REFUSED WHOLE and reported as a transport failure, never truncated into a shorter observation - the read takes one byte more than the limit precisely so the overflow is detectable, since a limited reader ends a truncated stream and a complete one the same way, and a document complete at exactly the limit followed by arbitrary bytes would otherwise decode from its prefix and be recorded as a whole observation of a response never read whole) (for a BUSINESS caller the shared transport special-cases 401/403 in the round trip and the transient 429/5xx statuses one layer down in the retry loop, and hands back every other non-2xx JSON body verbatim; the diagnostic read's own transport branch drops such a body instead, and the status guard in the reader is kept as defence in depth so a 400, a 404 or a redirect page carrying a `data` object could never be recorded as evidence even if that body were ever handed back), a non-2xx also DROPS the body and STOPS the client-ID candidate loop before the body is tested for `PersistedQueryNotFound` — dropping it matters as much as stopping, because returning it walked the response past the pre-decode value bound into `encoding/json`: measured on one 1,048,567-byte dense array, 44.9 MB at HTTP 404 against 3.2 MB for the identical body at HTTP 200, a 14x amplification chosen purely by the peer's status code (method for every allocation figure in this paragraph: the `runtime.MemStats` `TotalAlloc` delta around one `ObserveWatchStreakMilestone` call against the named fixture, as `TestAnOversizedResponseIsRefusedBeforeItIsDecoded`, `TestADecodeErrorCannotDisableTheValueBound`, `TestAContainerHeavyResponseIsAlsoRefusedBeforeDecoding` and, for the 200-versus-404 comparison, `TestQ3NonSuccessBodyIsAlsoBounded` in `internal/twitch` do; the before-repair figures describe code paths that no longer exist and are historical, not re-measurable); 401 and 403 are settled on the status alone and never read the body, so nothing downstream loses evidence (the structured detector reads the body and does not consult the status, so a rejected response whose body is shaped like a structured rejection would otherwise re-send the authenticated request under every shipped client ID and end the read as `UNSUPPORTED_QUERY` — three times the requests, naming the wrong cause; 401 and 403 keep their own handling, and transient statuses never reach this point because the retry schedule has already exhausted them), a raw body carrying DUPLICATE JSON object members — and ONLY that, in a document that parses whole, since the duplicate scan stops at the first repeated key and has therefore seen only a PREFIX, so the remainder is validated before the duplicate verdict is accepted — validated for DECODABILITY rather than syntax, since `json.Valid` accepts `1e10000` as a well-formed number while the decode that follows cannot put it in a `float64`, and the operative question is whether the value the rest of the read works with can exist at all; this is affordable exactly here because the value bound has already passed, whereas the value scan itself runs before any bound is established and can only afford a syntax check; merely malformed input keeps its own class — a 200 whose body is empty or does not decode is recorded under the generic `TRANSPORT` class, the decoder's own refusal, pinned by the malformed rows of `TestMalformedJSONKeepsItsOwnFailureClass` — rather than being reported as a duplicate-member problem it does not have, which would also let a peer choose the recorded class with syntax alone — is refused as `AMBIGUOUS_JSON` BEFORE the body is tested for `PersistedQueryNotFound` — the order is load-bearing, because the structured detector decodes the body and `encoding/json` keeps the LAST duplicate member, so a benign `errors` array followed by an APQ-shaped one would otherwise be taken as authoritative APQ evidence, drive the candidate loop under every shipped client ID, and end as `UNSUPPORTED_QUERY` (and the reverse member order would erase a real rejection into `NO_DATA_NODE`, so the peer would pick the recorded class by member order) without ever reaching the refusal; an ambiguous body is not evidence of anything, the marker included; and the diagnostic read does not use that substring detector at all — it requires a STRUCTURED rejection (a top-level `errors` array, every element an object carrying the APQ marker as either `message` or the `extensions.code` spelling, with no non-null `data` member beside it (an empty `data` object is present and refuses the reading; an explicit `null` is absent, as it is for `error`, `extensions` and `extensions.code`); the two spellings are ALTERNATIVE evidence rather than independent ones, so where both are present the CODE decides — it is the machine-readable field and the message is prose beside it, and an error reading `{"message":"PersistedQueryNotFound","extensions":{"code":"UNAUTHORIZED"}}` is an authorization rejection wearing an APQ message, which taken on the message alone would replay the authenticated request under every client ID and record the real rejection as `UNSUPPORTED_QUERY`; extensions carrying no code at all — or an explicit `"code": null`, which says "no code" rather than "a contradicting code" — fall back to the message, which is then the only evidence there is; presence and SHAPE are asked separately, because one type assertion answers both at once and so answers neither — a present but non-object `extensions` would read as an ABSENT one and fall through to the message — while an explicit `null` is treated as absent rather than malformed, since it says "no extensions object" and refusing it would risk missing a genuine rejection, the one direction where being wrong hides a stale shipped hash instead of merely refusing a response; and a PRESENT, NON-NULL top-level `error` member, which sits BESIDE the `errors` array rather than inside it, refuses the APQ reading outright — non-null because an explicit `null` is absent here, as it is for `data`, `extensions` and `extensions.code`, and copying `strictPersistedQueryNotFound`'s blunter any-present-key test cost evidence in the one direction that matters: `{"error":null,"errors":[{"message":"PersistedQueryNotFound"}]}` was refused as APQ and recorded `GRAPHQL_TOP_LEVEL_ERRORS`, so a null costing the peer nothing to add would conceal the stale shipped hash this operation exists to detect; that mutation-replay detector can afford to be blunter, this one cannot, so an APQ-shaped array presented alongside an explicit rejection cannot conceal it), because with the substring test any valid response carrying the marker in an unrelated string is resent under every candidate client ID and then reported as `UNSUPPORTED_QUERY`, DESTROYING the real milestone data it carried; both spellings are accepted because a genuine `PersistedQueryNotFound` is the expected steady state for this operation and failing to recognise one would replace an honest `UNSUPPORTED_QUERY` with `GRAPHQL_TOP_LEVEL_ERRORS` after a single dispatch (the unrecognised body still carries a non-empty `errors` array, which the reader refuses before it ever looks for a data node), hiding the stale shipped hash among ordinary service errors; this is deliberately not `strictPersistedQueryNotFound`, which authorizes a mutation REPLAY and demands the extensions code specifically (`encoding/json` keeps the last value, so an explicit `errors` rejection followed by an empty duplicate decodes with the rejection erased; the fail-closed checks below cannot see it, because they run against the already-lossy decoded map — `diagnosticJSONHasDuplicateMembers`, which reuses the bonus-mutation path's raw token scan (`scanJSONValueForDuplicateKeys`) but, unlike that path's `jsonObjectKeysAreUnique`, leaves a body that does not decode to the decoder instead of reporting it as a duplicate), a 401 keeps `UNAUTHORIZED` rather than the generic status class no matter what became of the body, and conversely `UNAUTHORIZED` is refused at every non-2xx that is NOT 401 — `isAuthError` also answers true for a body carrying the exact string `Unauthorized`, which is sound for a business read whose responses are not attacker-shaped text and wrong for a diagnostic read of an untrusted one (reproduced: HTTP 404 with `{"error":"Unauthorized"}` and HTTP 400 with `{"errors":[{"message":"Unauthorized"}]}` both recorded `UNAUTHORIZED`, letting the peer's BODY pick the class the status had already settled); `isAuthError` itself is untouched, since the business paths must still drive credential recovery from it, since a token rejection is settled by the status alone and needs no payload; and the same rule holds at a 2xx, where the shared round trip settles authorization on the status alone for a diagnostic read, so a 200 whose body merely says `Unauthorized` is recorded as `GRAPHQL_TOP_LEVEL_ERRORS` and never as a token rejection the peer chose — otherwise a rejection whose body merely happened to exceed the size limit would fail the read before the transport reached its 401 branch and be recorded under the status class instead, letting an attacker-chosen body SIZE decide which failure is recorded, a milestone node whose collections carry more than 4096 elements in total — `missedStreams` plus every nested `broadcastIdentifiers` — is refused as `OVERSIZED_COLLECTION` BEFORE the parse runs (for entries of the documented shape, `{"broadcastIdentifiers":[{"id":…}]}` at about six JSON values each, the 8192-value scan described below is the operative cardinality bound and refuses at roughly 1,360 entries, under the same class; the 4096-element bound is reached first only by degenerate elements such as empty objects), because the 1 MiB limit bounds BYTES and not cardinality and the two are not the same bound: a dense array of tiny elements turns a megabyte of wire into hundreds of thousands of retained Go structs (measured: a 1,048,575-byte body of `0,` elements produced 524,244 retained entries and ~145 MB allocated, on the bonus poll goroutine, once per started target — at most three per cycle under the allowance) while buying nothing, since the record prints exact counts and a bounded identifier sample and every element past the sample exists only to be tallied — it is a refusal rather than a truncation because a partly-walked array would report an exact count beside a tally taken over only some of its elements, which reads as evidence while being none; the limit is sized by margin rather than by observation — no live `RewardList` response has been captured (acceptance is `PENDING`) and the donor's model covers only the `watchStreakMilestone` subtree, so the cardinality of a real response is unverified and the limit is only expected to sit far above it — and the count itself short-circuits, so the check cannot become the amplification it prevents; this bounds what the PARSER retains, and a third limit bounds the decode that precedes it: a diagnostic body carrying more than 8192 JSON values — scalars, object keys AND opening containers, since the containers are precisely what `encoding/json` allocates, and counting only scalars leaves the cheapest amplification of all uncounted because a container-heavy body is nothing but delimiters (measured: 349,496 empty objects inside a just-under-1 MiB body scanned as ZERO values and still cost ~88 MB to decode, versus ~2 MB once containers count) — is refused under the same class by a STREAMING scan of the raw bytes, before `encoding/json` sees it — without that, the collection limit read an already-materialised document and the refusal cost ~97 MB and ~300 ms anyway (measured; ~3 MB after), because a refusal is only cheap when it runs before the expensive part; the scan holds only the decoder nesting stack and stops at the limit, and it decodes numbers with `UseNumber` so a value the decoder could never represent — `1e10000` — cannot end the scan and thereby switch the bound off for everything after it, which is what made a 240 KB body inside the byte cap report "within limit" and cost ~22 MB to refuse (~1.5 MB after): the SIZE question is kept separate from the REPRESENTABILITY one. It answers "within limit" for malformed JSON rather than claiming a size problem it did not observe, leaving the shape to the decoder that follows — including when the limit is crossed by a well-formed PREFIX of a malformed body, which is settled with `json.Valid` rather than by finishing the token loop, because finishing it boxes every value read and measured 54 MB on a 1 MiB body, reintroducing the very cost this scan exists to avoid; without that check a peer could choose between the size class and the shape class simply by moving its syntax error to either side of the limit boundary; declining to NAME a size class for a malformed body is however not the same as declining to BOUND it, and conflating the two cost the bound its teeth once already — an over-limit malformed body was reported as a plain "within limit", the duplicate-member scan that runs next then walked and boxed the whole payload, and a 1,048,560-byte dense array with ONE trailing invalid byte allocated 55.2 MB, the exact cost this scan exists to avoid, reachable by appending a byte; such a body now keeps its malformed class AND skips the duplicate scan, which loses nothing, since a body that does not decode cannot be recorded as an observation either way; the value limit (8192) is numerically above the collection limit (4096) but both refusals record the same `OVERSIZED_COLLECTION` class with no snapshot, so the number is not an ordering of evidence: the value scan runs first because it is cheaper, refusing before anything is decoded, and for entries of the documented shape it is the bound that actually fires; and a PRESENT `errors` node that is not an array is refused as `MALFORMED_ERRORS_NODE` (a GraphQL `errors` node is valid only as a list, and the generic non-empty-array test answers false for an object, a string, a number or null, so a rejection wearing the wrong shape would be walked past). A present, empty `errors` array reports no error and does not refuse the observation. A present, non-null top-level `error` member — the SINGULAR spelling, which sits BESIDE the array rather than inside it — is itself refused as `GRAPHQL_TOP_LEVEL_ERRORS` before the data node is parsed. The APQ detector already refuses to read APQ evidence out of a body carrying one, and leaving the observation path silent about it made the two disagree: `{"error":"Forbidden","data":{…}}` was recorded as `OBSERVED`, so a peer could attach an explicit rejection to forged data and still have the data retained as evidence. An explicit `null` is treated as absent for that key, as it is for `data`, `extensions` and `extensions.code`; the plural `errors` node is the deliberate exception — a `null` there is a malformed list (`MALFORMED_ERRORS_NODE`) — and the milestone parser itself records `null` as its own `NULL` classification rather than as absent. A failure (unsupported hash, transport error, cancellation, top-level GraphQL errors, partial response) yields `UNKNOWN`/`UNAVAILABLE` evidence only: it changes no `Stream` state, no Channel Points capability, no pursuit or recovery, no selection/rotation/broker state, and blocks no claim or redemption path.
  - **The milestone value node is string-encoded; the number form is accepted as tolerance.** `RewardList` is not uniform about its integers: `watchStreakThreshold` and `watchStreakCopoBonus` arrive as JSON numbers, while the nested `watchStreakMilestone.value` arrives as a JSON *string* (donor protocol evidence, pinned to `mpforce1/Twitch-Channel-Points-Miner` ref `f1dda17a…`, the same ref the `RewardList` operation itself is sourced from: `ViewerMilestone.value` is a `str`, read with `expect_str`, while the sibling fields use `expect_int`; recorded from owner-supplied evidence 2026-09-07 and corroborated against the donor's `Parser.py` at that ref. The number-encoded form is accepted as tolerance and is **not** donor-attested for this field). The parser therefore accepts either encoding **for `value` only** — missing => `MISSING`, `null` => `NULL`, a string of decimal digits => `VALID` + the parsed integer, a JSON integer => `VALID` + that integer — and records which one arrived in a separate `WireKind` slot (`NUMBER`/`STRING`, surfaced as `milestoneValueWireKind`), because `"4"` and `4` are the same observed integer but not the same wire fact and neither answer may hide the other. Anything else stays `MALFORMED`: a bool, object or array, a non-integral number, a string that is not a signed run of decimal digits (`"four"`, `""`, `"4.0"`, `" 4"`, `"0x4"`, `"1,000"`), **and a digit run too large for a platform `int`** — `strconv.Atoi` returns the *saturated* magnitude alongside its range error, so an out-of-range run such as `"9223372036854775808"` is refused rather than recorded as `MaxInt`. Normalisation keeps the integer and the encoding but not the lexical form: `"4"`, `"+4"` and `"0004"` all report `4`/`STRING`. Nothing is fabricated for a node that was not validly read — no value and no wire kind. Within the platform-`int` bound, exactness follows the encoding: a JSON *number* is refused at and above 2^53 because `float64` decoding has already lost the identity of what was sent, while a decimal *string* has lost nothing and is read exactly. On the 64-bit builds this project ships, the two encodings therefore disagree between 2^53 and `MaxInt`; on a hypothetical 32-bit build the platform bound bites first and both refuse near 2^31, so the disagreement is a property of the wider `int`, not a universal invariant. The tolerance is protocol tolerance only and grants no interpretation: it does **not** make `value` the streak count, a ladder rung, or a link to any grant, and the sibling number-encoded fields keep rejecting strings.
  - **Diagnostic requests are isolated from connectivity accounting.** The observation is issued as a *diagnostic* request (`internal/twitch`'s `diagnosticRequestKey`), which the shared read transport excludes from `TwitchClient.ConnHealth` in **both** directions: it records no functional or transport failure, refreshes no success timestamp, drives no credential recovery or operator reauth escalation, and raises no `WARN`/`ERROR` for its own request outcome (stale hash, retries and exhaustion all log at `DEBUG`). The isolation extends to the shared client-ID pool. A diagnostic read caches **nothing**: it neither promotes the **process-wide default** nor pins a per-operation candidate, and it never raises the operator `WARN` announcing that the persisted-query hashes are stale. It pinned a per-operation candidate in earlier revisions, guarded by a predicate that tried to decide IN THE TRANSPORT whether the reader would accept the response. Every version of that predicate was an incomplete restatement of the reader's rules, and three consecutive review rounds each found a body it admitted and `ObserveWatchStreakMilestone` then rejected: an explicit service rejection, a non-object `data` node, and an oversized collection. Because `candidateClientIDs` places a cached ID FIRST and any non-`PersistedQueryNotFound` response ends the candidate loop, each of those pinned a candidate permanently and the shipped default was never retried again. Predicting one layer's verdict in another is the defect rather than any one of those shapes, so the prediction was removed rather than extended a fourth time. What it bought was at most one saved request per target per cycle, and only in a state — the shipped default failing while a fallback serves — that `RewardList` has never been observed in, its acceptance being `PENDING`; in the state it IS expected to be in, every candidate answers `PersistedQueryNotFound` and nothing was ever cached. A **business** read caches and promotes exactly as before. That default is shared: `candidateClientIDs` hands it to every *uncached* operation, business ones included, and `ActiveClientID` surfaces it in the Health Center. Promotion fires whenever a fallback candidate answers anything that is not `PersistedQueryNotFound` — a `401` included — so without the split a diagnostic observation that **failed** would still steer the client ID a later bonus claim goes out under, and would still tell the operator to update `internal/constants/gql.go` when nothing had resolved. That is this operation's expected shape rather than a corner case, because `RewardList` acceptance is `PENDING`. A **business** rotation still promotes and still raises that `WARN`, unchanged. This is load-bearing, not cosmetic. `ConnHealth.RecentFunctionalFailures` reaches `internal/miner.classifyAPI`, which turns two failures into `health.SignalGQLAPI = DEGRADED`, which the auto-bet health gate turns into "no automated prediction bets" — so without the isolation a `RewardList` hash Twitch does not accept (the outcome that is *expected* until live acceptance is evidenced) would reach the two-failure degrade threshold on the second unaccepted traversal — the second cycle — and close the auto-bet gate permanently from then on. The suppression is symmetric so a succeeding observation can no more mask a real outage than a failing one can invent it, and it applies **only** to the diagnostic read: every business operation's accounting, recovery and stale-hash `ERROR` are unchanged.
  - **The reward ladder is a current official Twitch fact; the streak-count field is not.** Twitch's [Viewer Channel Point Guide](https://help.twitch.tv/s/article/viewer-channel-point-guide) documents the ladder — streak count **2 → +300, 3 → +350, 4 → +400, 5 or more → +450** — along with the rule that a qualifying stream must run at least 10 minutes and at least 30 minutes must have elapsed since the previous stream ended. Recorded 2026-09-06 from owner-supplied evidence. The donor `mpforce1/Twitch-Channel-Points-Miner` (ref `f1dda17a…`, README blob `d5b60c22…`) corroborates only the four AMOUNTS; the streak-count→amount mapping, the flat 5-or-more rung and the 10/30-minute rule rest on the Twitch guide alone. The guide states earn rates are **subject to change**, so this is a dated current-official fact, not a permanent invariant, and the 10/30-minute rule is recorded as Twitch's stated rule only — this miner's pursuit semantics are unchanged and are not derived from it.
    What remains **UNKNOWN** is which `RewardList` field, if any, carries the authoritative streak count: `value`, `watchStreakThreshold`, `watchStreakCopoBonus`, `state` and milestone IDs keep their raw observed meanings until source or runtime evidence proves otherwise. Knowing the ladder is not knowing where the rung number lives.
    The correlation record therefore *labels* an amount against the ladder rather than measuring a streak: it reports consistency (`CONSISTENT_WITH_STREAK_COUNT_2/3/4/5_OR_MORE`, `OFF_DOCUMENTED_BASE_LADDER` for a real amount matching no documented rung, or `AMOUNT_UNKNOWN` when the frame carried no exactly representable amount to compare) and keeps `provenStreakCount` at `UNKNOWN` on every record, including a `+450` one — the top rung is flat, so an amount cannot distinguish a 5th streak from a 50th. Because the ladder documents **base** amounts, the label is computed from the frame's own `baseline_points` where present and exactly representable (a channel-points multiplier scales the credited total above the base; otherwise the credited total is compared) and names the basis it used — the field's PRESENCE is evidenced by this repository's `points-earned` fixtures (`point_gain.total_points`, `baseline_points`, `reason_code`, `multipliers`, with `baseline_points == total_points` when `multipliers` is empty); its pre-multiplier MEANING is an assumption drawn from the field's name and the `multipliers` array beside it, pending a captured multiplied frame, and is not an attested wire fact. The label is never a filter: every valid integer `WATCH_STREAK` amount passes through the existing accounting path unchanged, and one matching no documented rung is reported as off-ladder rather than filtered, rewritten, promoted or rejected.
  - **No historical reconstruction.** Past milestones are not reconstructed, inferred or backfilled, and a received amount never establishes a historical streak-count binding — knowing the ladder does not license inventing which rung a past grant sat on. Repeated identical milestone values observed at different times are distinct observations and are never deduplicated away; ordering is by request-start sequence, so an older request that completes later can never be presented as newer evidence. `achievementTimestamp` belongs to the observed achievement field and is recorded separately from the local request-start/request-end sampling window.
  - **No authoritative grant-to-broadcast binding.** The `WATCH_STREAK` PubSub frame still carries no provable `BroadcastID`; the currently observed `Stream.BroadcastID` is logged as local context only and is never promoted to `ProvenBroadcastID`. A milestone snapshot taken before or after a grant is only a PREVIOUS OBSERVATION or a FOLLOWING OBSERVATION — never "expected at grant" or "changed because of grant". Delayed PubSub, overlapping or reverse-completing requests, clock disagreement, post-grant-only sampling, restart, streamer remove/re-add and broadcast change all leave that relationship explicitly `UNKNOWN`.
  - **Grant correlation reports what actually happened.** For a newly accepted grant the record states the domain admission and the exact ledger outcome as separate facts — `LEDGER_COMMITTED`, `LEDGER_DUPLICATE`, `LEDGER_FAILED`, `TIMELINE_ONLY` or `ANALYTICS_UNAVAILABLE` — using the already-existing event identity and event-local `total_points`. `COMMITTED` is never reported from domain acceptance alone, and the exact accounting/admission/timestamp semantics of the points ledger are unchanged.
  - **Business-first deadline, roster cursor and cycle allowance (owner decision 2026-09-08; design residue D1/D2/D4 of the owner's PR #315 design-residue audit).** Three bounded, loop-local mechanisms, implemented with no new scheduler, worker, ticker, negative/positive cache, persistent store, ledger, dependency or user setting:
    - *Deadline (D1).* The stage still runs synchronously on `bonusPollLoop`, strictly after `pollBonuses` returns, and never resets, drains or re-arms the business ticker. Its deadline is the earliest of `stageStart + milestoneObservationCycleBudget` (40s), `tick + bonusPollInterval` (60s) and the owner context's deadline — on an exact tie the source is reported in that order (`STAGE_BUDGET`, then `NEXT_TICK`, then `OWNER`; the comparisons are strict) — where `tick` is the timestamp carried by the serviced `time.Ticker` event — the SCHEDULED fire time, so a tick read late behind a long business pass still reports when it was due (`milestoneStageDeadline`, a pure function pinned with literals). A cycle whose business pass consumed the whole period, or an overdue/coalesced tick, has no slack and admits no diagnostic request; it is never granted a fresh 40s (for a coalesced tick, tick + period is a lower bound on the real next tick and already past, so the cycle serviced on it admits nothing even if some real slack remains before that tick fires). Slack shorter than one round trip is still admitted: the first dispatch is charged and released by the deadline, and the record reads `CANCELLED`/`DEADLINE_EXCEEDED`. This is a scheduling bound, not a real-time guarantee — an in-flight request is released through its context, not pre-empted — and the business pass itself remains unbounded on this loop; the rule only guarantees the stage never compounds it. The owner accepted that on exactly the cycles where a slow Twitch makes the evidence most interesting, business polling wins.
    - *Cursor (D2).* One non-persistent integer cursor lives on the `bonusPollLoop` stack and nowhere else: no retained streamer pointer, no per-target map, no store. Each cycle snapshots the roster (`Manager.All()`, unchanged) and normalizes the cursor modulo its length (an empty roster resets it to 0), then visits each eligible target (online AND non-empty `ChannelID`) at most once, in roster order from that position. The cursor advances past the last target that actually STARTED (made at least one dispatch), whatever the outcome — success, error, timeout or APQ — so a slow or expensive first target cannot monopolize the roster across cycles; a target refused before its first dispatch keeps its turn and is not reported as observed: the cursor is left ON that target, so the ineligible entries skipped to reach it are not revisited first; ineligible entries are skipped and never move the cursor on their own; a cycle that admitted nothing at all (no slack) leaves the cursor exactly where it was; and a target whose channel identity disappears after the eligibility mask was taken is skipped without ending the roster, without a record and without moving the cursor on its behalf. Because it is a position rather than an identity, removing an entry that sits BEFORE the cursor shifts the roster under it: the entry that slides into the position is served and the one that slid past it is served only when the cursor comes round to it again — up to one full round of the remaining roster later (one started target per cycle in the unaccepted-hash steady state, up to three per cycle with an accepted hash) — the documented cost of owning no per-streamer state. Business ordering, watch selection and Broker state are untouched.
    - *Allowance (D4).* Exactly three explicit, authenticated diagnostic HTTP attempts per logical cycle (`milestoneCycleDispatchAllowance`), shared across all targets, retries and client-ID fallback. The permit unit is one `http.Client.Do` on the diagnostic path; it is charged once, immediately before the dispatch, never refunded on error or timeout and never refilled within the cycle. Remaining allowance is checked WITHOUT being consumed before another target (stage), another candidate (fallback loop) and another retry (before the backoff wait), so exhaustion never triggers an additional request or a pointless retry wait; it is reported as its own class — `UNAVAILABLE`/`ALLOWANCE_EXHAUSTED` for a target that was started and stopped, `SKIPPED`/`ALLOWANCE_EXHAUSTED` for one refused on entry — never as a transient transport failure and never into connectivity accounting, and an owner cancellation landing on the last permit still reads `CANCELLED`, because the transport consults the owner's context before the allowance at both of its check points (before a retry wait and before the next client-ID candidate). A partial candidate traversal is `UNKNOWN`/incomplete evidence: it is NOT proof that every client ID rejected the query, so `UNSUPPORTED_QUERY` remains valid only after an actual complete APQ exhaustion, which costs exactly one permit per shipped candidate — three — and therefore still fits one cycle's allowance (`TestRewardListCandidateSetFitsTheCycleAllowance` in `internal/twitch` fails first if a fourth shipped candidate ever appears, and `TestTheShippedClientIDSetFitsTheCycleAllowance` in `internal/miner` fails first if the allowance is ever lowered below the shipped set). The diagnostic path also never follows a redirect: it dispatches through a request-local copy of the shared client whose `CheckRedirect` returns `http.ErrUseLastResponse` (the precedent is the `ClaimCommunityPoints` transport), so a 3xx keeps its status-based `HTTP_STATUS` refusal and can never become a second authenticated dispatch, while the shared client and every business read follow redirects exactly as before. This is an APPLICATION attempt cap chosen as a local load policy; it is not a Twitch quota and not a wire guarantee. The owner explicitly accepted the loss of diagnostic completeness and freshness it buys — on a large roster a target is observed at best once every ⌈eligible/3⌉ cycles, and once every `eligible` cycles in the unaccepted-hash steady state — in exchange for business cadence and a finite request count.
  - **Resource bounds run before APQ recognition — an accepted limitation, not a repaired defect (owner decision 2026-09-08; design residue D3 of the same audit).** The diagnostic read applies its byte bound, its JSON value bound and its duplicate-member scan BEFORE it tests a body for the structured `PersistedQueryNotFound` rejection, and its collection bound before the parse. An APQ rejection carried inside a body that exceeds one of those bounds is therefore refused under the bound's own class (a transport failure for the byte limit, `OVERSIZED_COLLECTION` for the value limit; the collection bound is never reached by an APQ rejection, which carries no `data` object — a body carrying both an APQ `errors` array and an oversized `data` collection that still fits the 8192-value bound is refused as `GRAPHQL_TOP_LEVEL_ERRORS` by the top-level errors check that precedes the collection bound; one that also exceeds the value bound is `OVERSIZED_COLLECTION` from the value scan, as the preceding clause states) and never drives client-ID fallback; the specific APQ name is lost on such a body. That order stays: reversing it would let a response's SIZE decide whether the expensive path runs, which is the amplification the bounds exist to prevent, and none of the donors examined for this decision offered a mechanism or test for the alternative (the examination is recorded in the owner's design-residue audit, which is not part of this repository). The loss adds no false `OBSERVED`, no fabricated zero and no false `UNSUPPORTED_QUERY`; the cycle allowance does not depend on APQ specificity; no parser redesign is planned, and any further APQ specificity would be a separate owner decision.
  - **Privacy.** Records carry bounded, allowlisted provenance only. Raw GQL payloads, OAuth tokens or cookies, request/response headers, arbitrary raw error strings (failures are reduced to a bounded class), credentials and playback tokens/signatures are never logged. That holds for the shared transport's own retry trace too: a diagnostic retry is not merely lowered to `DEBUG` but is stripped of the transport error text, because `FileLevel` defaults to `DEBUG` and a lowered record still reaches the retained log — the bounded operation, attempt and status are kept, the unbounded error string a hostile redirect or proxy could shape is not. A business retry keeps its full trace, where the raw error is actionable.
- **Bounded streak deferral:** when the current fair pair is about to lose an in-progress streak member, the loop may set one explicit `deferUntil = now + 2 min` for that pair approach. Re-evaluation cannot extend the deadline; expiry forces reconciliation, and a member that goes offline or loses protection leaves immediately. It is reached only when an ordinary replacement is actually pending: while the committed ordinary cohort still holds every base seat there is none to defer, so the one-shot approach is neither armed nor consumed; a cohort holding only *some* of the seats is different, because the seats it does not hold are re-ranked on that very evaluation. `PairSince` changes only when pair membership actually changes — it is the base-pair membership timestamp, and is deliberately no longer the anchor the minimum residence is measured from (see above). This best-effort protection does not cover imminent drop-campaign completion and never overrides a strictly stronger hard class.
- **Retired configuration compatibility:** old `config.json` files containing `rateLimits.rotationInterval`, `rotationIntervalMinMinutes`, or `rotationIntervalMaxMinutes` still decode because unknown JSON keys are accepted. Those values have no runtime owner or effect and `SaveConfig` never serializes them. Runtime Settings, the Settings page, diagnostics, and the sidebar expose no retired fields or future-rotation projection. The 15-minute minimum residence does not revive them: it is a fixed internal constant, not a configurable interval, and it projects no future rotation.
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
  the window) accumulate short of a full blackout. Business reads only: the
  diagnostic `RewardList` observation is excluded from this accounting in both
  directions (see *Watch Streak milestone observability*).
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

-- Exact point-event ledger (migration v5) — one row per ACCEPTED
-- points-earned event, the accounting authority behind the Statistics
-- earnings breakdown (see "Exact Point-Event Ledger"). event_id is the PubSub
-- EventFingerprint (SHA-256 of topic + canonical inner message), so
-- UNIQUE(event_id) makes RecordPointEvent idempotent against an exact
-- re-delivery. total_points is the event-local point_gain.total_points Twitch
-- granted; balance_after is the balance.balance the same frame reported (NULL
-- when the frame carried none). points_id is the balance-timeline sample
-- (points.id) written in the same transaction, which is how a sample is
-- recognized as exact-backed. No FOREIGN KEY (same reasoning as v4). Unlike
-- prediction_bets this table IS part of the retention sweep. Additive only:
-- no ALTER of existing tables and no backfill of pre-ledger history.
CREATE TABLE point_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    streamer_id   INTEGER NOT NULL,
    event_id      TEXT NOT NULL UNIQUE,
    timestamp     INTEGER NOT NULL,
    reason_code   TEXT NOT NULL,       -- raw Twitch reason_code (WATCH, CLAIM, ...)
    total_points  INTEGER NOT NULL,    -- event-local amount granted by this event
    balance_after INTEGER,             -- frame balance, NULL when unknown
    points_id     INTEGER NOT NULL     -- the timeline sample this event produced
);

-- Immutable Prediction observation trail (migration v6) — an append-only
-- record of what the Prediction subsystem SAW and DECIDED, in two tables: one
-- control row per collector run and one immutable fact per observation. It is
-- a pure observer. The trail never places, modifies, gates or delays a bet,
-- never changes a Twitch call's count, arguments or result, and never feeds
-- the points ledger or any dashboard figure; a fact that cannot be captured or
-- written is dropped and its session is finalized INCOMPLETE, so a failure is
-- always borne by the trail alone.
--
-- A fact is recorded WHOLE or not at all. Every frozen ceiling -- identifier
-- length, outcomes per fact, predictor cohort, payload bytes, rows and bytes
-- per round, per deletion identity and per store -- refuses the observation
-- rather than shortening it, because a shortened fact is not a smaller truth:
-- a truncated channel id names a different channel, and a round stored with 64
-- of its 70 outcomes is indistinguishable from one that had 64. The ceilings
-- are checked BEFORE the insert, against current usage plus the incoming fact,
-- and the per-identity ones are what bound the cost of a privacy erasure in
-- advance. Every refusal is counted, which is what makes the session
-- INCOMPLETE and says so.
--
-- collector_sequence is reserved when a fact is CAPTURED, not when it is
-- written, so a loss leaves a gap: committed_count + dropped_count accounts for
-- every position the session handed out, and a reader that finds otherwise
-- treats the session as an integrity error rather than as authoritative.
--
-- One collector session exists per process run. collector_epoch is allocated
-- by a committed INSERT and LastInsertId — never MAX(epoch)+1, which would
-- reuse an epoch after a deletion and silently merge two runs — and the row is
-- finalized exactly once, by a single compare-and-set from OPEN.
CREATE TABLE prediction_observation_sessions (
    collector_epoch                   INTEGER PRIMARY KEY AUTOINCREMENT,
    collector_session_id              TEXT NOT NULL UNIQUE,
    producer_revision                 TEXT NOT NULL,   -- producer contract the rows were written under
    started_at_ms                     INTEGER NOT NULL,
    closed_at_ms                      INTEGER,         -- NULL iff still OPEN
    close_state                       TEXT NOT NULL,   -- OPEN | COMPLETE | INCOMPLETE | ABANDONED
    last_assigned_sequence            INTEGER,
    committed_count                   INTEGER NOT NULL,
    dropped_count                     INTEGER NOT NULL,
    unsettled_obligation_count        INTEGER NOT NULL,
    post_fence_producer_count         INTEGER NOT NULL,
    producer_shutdown_uncertain_count INTEGER NOT NULL
);

-- One immutable fact per observation. Facts are INSERT-only: there is no
-- UPDATE, REPLACE or upsert anywhere on this table, and the only deletions are
-- retention and an explicit privacy erasure. UNIQUE(collector_epoch,
-- collector_sequence) makes one session's causal order total and
-- gap-detectable. Every parent id requires its corresponding channel id, so no
-- row can exist that a channel-scoped erasure cannot reach, and
-- round_incarnation_id exists exactly when retention_group_owner_channel_id
-- does, which is what makes whole-round retention well defined. It names one
-- LOCAL ADMISSION -- the pool instance that admitted the round plus that
-- pool's admission counter -- not a Twitch event: a round cleaned up and
-- created again, or admitted by a rebuilt pool, is a different local round,
-- and the retention unit is the compound (collector_epoch, pool_instance_id,
-- round_incarnation_id) that retention groups and deletes by. event_id and
-- source_fingerprint are deliberately NOT unique: one round produces many
-- facts, and a duplicate delivery is itself a fact worth keeping. No FOREIGN
-- KEY (same reasoning as v4 and v5); parents are resolved lookup-only, so an
-- observation never creates a `streamers` row. payload_json holds a closed,
-- typed, sanitized projection — never a raw PubSub/GraphQL body, a
-- Topic.String(), the transport EventFingerprint, a token, a raw error or a
-- predictor identity — and observation_sha256 digests that projection.
-- Additive only: no ALTER of existing tables and no backfill.
CREATE TABLE prediction_observations (
    id                                INTEGER PRIMARY KEY AUTOINCREMENT,
    observation_id                    TEXT NOT NULL UNIQUE,
    collector_session_id              TEXT NOT NULL,
    collector_epoch                   INTEGER NOT NULL,
    collector_sequence                INTEGER NOT NULL,
    pool_instance_id                  TEXT NOT NULL,   -- producing pool instance
    round_incarnation_id              TEXT,            -- this pool's LOCAL admission of a round
    routed_streamer_id                INTEGER,
    routed_channel_id                 TEXT,
    round_owner_streamer_id           INTEGER,         -- provenance; never expands deletion
    round_owner_channel_id            TEXT,
    retention_group_owner_streamer_id INTEGER,
    retention_group_owner_channel_id  TEXT,
    round_capture_origin              TEXT,            -- ACTIVE_AT_ADMISSION or
                                                       -- PREFIX_UNOBSERVED_AT_ADMISSION, FROZEN when
                                                       -- the round was admitted and repeated on
                                                       -- every fact about it
    round_capture_gap_cause           TEXT,            -- closed enum, set exactly when the prefix
                                                       -- went unobserved: STARTING, DISABLED,
                                                       -- NO_SINK, IDENTITY_FENCE, CLOSING, CLOSED
    event_id                          TEXT,            -- NOT unique: many facts per round; only
                                                       -- ever set on a fact a channel-scoped
                                                       -- erasure can reach (routed or retention
                                                       -- group channel), never on one it cannot
    kind                              TEXT NOT NULL,   -- one of the nine closed kinds
    source_topic_type                 TEXT,            -- topic TYPE only; closed enum over the two
                                                       -- proved Prediction classes, with no UNKNOWN
                                                       -- member: an unrecognized class is stored as
                                                       -- no claim at all
    source_message_type               TEXT,            -- closed enum: the four Prediction message
                                                       -- types, or the wire state that says why
                                                       -- none is there (ABSENT_ON_WIRE,
                                                       -- NULL_ON_WIRE, INVALID, UNKNOWN_PRESENT);
                                                       -- the unrecognized value is never stored
    source_fingerprint                TEXT,            -- own digest, NOT the transport one
    producer_at_ms                    INTEGER,
    producer_time_source              TEXT NOT NULL,   -- where producer_at_ms came from; a frame
                                                       -- timed by the receiver's own clock records
                                                       -- RECEIVER and no producer time at all
    received_at_ms                    INTEGER NOT NULL,
    connection_index                  INTEGER,
    connection_generation             INTEGER,
    connection_sequence               INTEGER,
    payload_version                   INTEGER NOT NULL,
    payload_json                      TEXT NOT NULL,   -- closed sanitized projection
    observation_sha256                TEXT NOT NULL,
    UNIQUE (collector_epoch, collector_sequence)
);

-- Indexes for performance
CREATE INDEX idx_points_streamer_time ON points(streamer_id, timestamp);
CREATE INDEX idx_annotations_streamer_time ON annotations(streamer_id, timestamp);
CREATE INDEX idx_chat_streamer_time ON chat_messages(streamer_id, timestamp);
CREATE INDEX idx_predbets_streamer_time ON prediction_bets(streamer_id, timestamp);
CREATE INDEX idx_point_events_streamer_time ON point_events(streamer_id, timestamp);
CREATE INDEX idx_point_events_points_id ON point_events(points_id);
-- Every role identity is indexed BOTH by its resolved parent id and by its
-- channel id, and each of those carries the round coordinates after the
-- identity, so identity work is scoped to an epoch, a pool and a round rather
-- than answered globally.
CREATE INDEX idx_predobs_exact_pair ON prediction_observations(collector_epoch, collector_session_id, collector_sequence);
CREATE INDEX idx_predobs_routed_parent ON prediction_observations(routed_streamer_id, event_id, pool_instance_id, round_incarnation_id, collector_epoch, collector_sequence);
CREATE INDEX idx_predobs_routed_identity ON prediction_observations(routed_channel_id, collector_epoch, pool_instance_id, round_incarnation_id);
CREATE INDEX idx_predobs_round_owner_parent ON prediction_observations(round_owner_streamer_id, event_id, pool_instance_id, round_incarnation_id, collector_epoch, collector_sequence);
CREATE INDEX idx_predobs_round_owner_identity ON prediction_observations(round_owner_channel_id, collector_epoch, pool_instance_id, round_incarnation_id);
CREATE INDEX idx_predobs_retention_parent ON prediction_observations(retention_group_owner_streamer_id, collector_epoch, pool_instance_id, round_incarnation_id);
CREATE INDEX idx_predobs_retention_identity ON prediction_observations(retention_group_owner_channel_id, collector_epoch, pool_instance_id, round_incarnation_id);
CREATE INDEX idx_predobs_round_unit ON prediction_observations(collector_epoch, pool_instance_id, round_incarnation_id, received_at_ms);
CREATE INDEX idx_predobs_null_round_epoch ON prediction_observations(collector_epoch, received_at_ms, id)
    WHERE round_incarnation_id IS NULL;
CREATE INDEX idx_predobs_received_at ON prediction_observations(received_at_ms);
-- Three indexes beyond that list, each for a reader this build ships:
-- ObservationsBySession looks a session up by its id alone; ObservationsByRound
-- looks an incarnation up by its id alone; and the bounded NULL-round prune
-- filters collector_epoch with an INEQUALITY before ordering by received_at_ms.
CREATE INDEX idx_predobs_session ON prediction_observations(collector_session_id, collector_sequence);
CREATE INDEX idx_predobs_round ON prediction_observations(round_incarnation_id, id);
CREATE INDEX idx_predobs_null_round_retention ON prediction_observations(received_at_ms, id)
    WHERE round_incarnation_id IS NULL;
CREATE INDEX idx_predobs_fingerprint ON prediction_observations(source_fingerprint);
```

`prediction_bets` is deliberately **excluded** from the retention sweep
(`PruneBefore` prunes `points`, `point_events` and `annotations`), so lifetime
ROI stays exact; it grows by one row per resolved prediction. Migrations v4, v5
and v6 are additive (no `ALTER` of existing tables, no data rewrite), so they
are safe to apply to a populated database, and a pre-v5 binary opening a v5
database skips the higher version, never touches `point_events`, and keeps
working on the tables it knows (its own `points` rows then read back as
legacy, not exact-backed). Its retention sweep and streamer purge do not know
`point_events` either, so a rollback can orphan ledger rows (a pruned sample,
a purged `streamer_id`); the current release tolerates them: an orphan of a
pruned sample remains an accepted earning until retention sweeps it, and an
orphan of a purged streamer is unreachable by login (ids are never reused)
until retention sweeps it. `DeleteStreamerTx` purges `point_events` together
with the other tables in the same transaction; a rename preserves them through
the stable `streamer_id`.

The v6 observation tables are **excluded** from `PruneBefore`. Their retention
is owned by the collector's own worker, which removes exactly one bounded unit
per transaction — one whole eligible round, at most 128 NULL-round facts, or
at most 128 factless finalized sessions — reusing the same
`Analytics.RetentionDays` setting rather than adding one. The active epoch is
never pruned, and a crash-left `OPEN` session is never pruned automatically:
it is the only durable evidence of an unclean shutdown. A privacy erasure runs
inside the existing streamer-purge transaction and is identity-scoped: a
retention-group-owner match removes the **whole round**, a routed-only match
removes **only** the matching fact, and `round_owner_*` never expands
deletion. A rename performs **zero** `UPDATE` on a fact — the trail is
immutable — and relies on the same stable `streamer_id` everything else does.

Downgrade below v6 is **not** symmetric with v5's. A pre-v6 binary reads and
writes a v6 database correctly (it skips the higher version and never
references either table), but its streamer purge and retention sweep do not
know these tables, so it **cannot complete a privacy erasure** of observations
already recorded: the facts survive the purge that removed the login's other
rows. Rolling back below v6 once observation data exists is therefore a policy
decision requiring a separate scrub or forward-only choice, not a safe default.

Readers must classify a session before drawing any conclusion from its facts:
`UNFINALIZED` (the session never wrote its accounting — `OPEN`, so live or
crash-left, or `ABANDONED`, meaning a later startup reclaimed a crash-left
row — so no absence may be inferred), `INTEGRITY_ERROR` (the row contradicts
itself), `ADMINISTRATIVELY_TRUNCATED` (finalized coherently, but facts were
removed afterwards by retention or an erasure), or `AS_FINALIZED` (the facts
present are exactly those the session committed).

`AS_FINALIZED` is a statement about the DATASET, not about completeness: an
`INCOMPLETE` session reads `AS_FINALIZED` too, because the facts it did commit
are still exactly present and exactly right. The absence of a fact is evidence
only when the reading is `AS_FINALIZED` **and** `close_state` is `COMPLETE`.

A session that dropped anything, left an obligation unsettled, or never opened
intake at all finalizes `INCOMPLETE`; its committed facts are still exact, but
the SET of facts is not provably whole. A session written under a different
producer contract is **not** an integrity failure — its rows are exactly what
that contract wrote, so the reading stands and the caller is told which
contract produced them; classifying it as an error would make the whole trail
unreadable the moment the revision is bumped.

A crashed process leaves its counters at the zeros they were inserted with,
because they are written only at finalization. Those zeros are not evidence,
so the startup reconciliation records the run `ABANDONED` and neither presents
them as the dead process's accounting nor reconstructs a replacement from the
surviving facts: that run's facts are a LOWER BOUND on what it observed, and
nothing about loss follows either way.

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

### Exact Point-Event Ledger

The Statistics earnings accounting (`exactBreakdown`) is computed from
**events, never from balance deltas** (the daily digest's *Net points*
remains, by definition, a net balance change and is out of the ledger's
scope; the compatibility `breakdown` field is the first release's
balance-delta attribution, kept unchanged and never used as accounting). Three kinds of facts
are kept apart and none is authority for another:

1. **Exact earning events** (`point_events`) — immutable accounting facts. For
   every points-earned event the PubSub pool ADMITTED (for `WATCH_STREAK`,
   only a newly accepted grant — the pool's admission stays the linearization
   point), the miner builds an event-local snapshot from the same frame: the
   event identity (`EventFingerprint`), the raw `reason_code`, the exact
   `point_gain.total_points`, and `balance.balance` when present (the ledger's
   `balance_after` is NULL when the frame carried none, or one that is not an
   exact integer). No earning or `balance_after` is ever re-read from the
   mutable `Streamer` (a poll or a later frame may already have moved its
   balance); only the chart's timeline sample falls back to the streamer's
   current balance, as a display value, when the frame carried no exact
   balance (absent, or present but not an exact integer).
   `timestamp` is the acceptance time stamped by the analytics service,
   consistent with every other analytics table. `Service.RecordPointEvent`
   writes the ledger row, its balance-timeline sample and — for
   `WATCH_STREAK`/`RAID` — the chart annotation in **one transaction**
   (`database.WithTx`, so the close barrier holds); `ON CONFLICT(event_id) DO
   NOTHING` is detected through `RowsAffected() == 0` and the whole
   transaction is rolled back on an exact re-delivery, so a duplicate identity
   leaves no second row, sample or marker and two concurrent deliveries yield
   exactly one winner. The ledger is a persistence invariant only — it is not
   a second replay controller.
2. **Balance timeline** (`points`) — absolute balance snapshots for the chart,
   tagged with the reason that caused the change. Points-spent frames and
   points-earned frames the ledger cannot admit (no identity; a `total_points`
   that is missing, non-numeric, non-integral or not strictly below 2^53 in
   magnitude, since a decoded float64 at that bound may have rounded from
   2^53+1 — never coerced to 0; a payload without an RFC 3339
   `data.timestamp`, whose fingerprint would not distinguish two equal grants)
   are recorded here only, at the frame's own balance when it carries an
   exact one (else the streamer's current balance, as before the ledger
   existed), still with their `WATCH_STREAK`/`RAID` marker when the amount
   is exact.
3. **Annotations** — display markers whose text (`+450 - Watch Streak`) is
   built from the same event-local amount; they are never parsed back into
   accounting numbers.

**Statistics breakdown.** `GET /api/points-history` aggregates exact earnings
in SQL (the `ExactEarningsBetween` aggregation, read through
`PointsSnapshotBetween`: `SUM` of positive `total_points` and `COUNT`
of positive events per canonical reason, plus `COUNT(*)` of all rows as the
event total, independent of the raw-series row cap). Unknown reason codes are
exact earnings pooled into `OTHER`; non-positive amounts are accepted facts
but never earnings. History recorded **before the ledger** (or by a pre-v5
binary) is not backfilled; it stays available only as a clearly separate
**legacy estimate** (`EstimateLegacyBreakdown`: positive deltas into samples
no exact event backs, skipping points-spent snapshots). The response carries
three separate lists that are never summed with one another:
`exactBreakdown` — the **authoritative accounting**, the ledger aggregation,
present whenever the range holds a positive exact event; `legacyBreakdown` —
the **explicit estimate** for the history no exact event covers (the
uncovered part of a *mixed* range, or the whole of a legacy-only range;
absent when unavailable or when nothing could be attributed); and
`breakdown` — the **compatibility attribution** of the first Statistics
release (`BreakdownFromSamples`, unchanged since base
`dc5566049f1de1909d66c0f190338d54af863402`: every consecutive positive
balance delta of the raw series attributed to the later sample's canonical
reason, first sample baseline, ordered gained-descending then
reason-ascending, computed over the raw series whether or not it was
truncated), kept byte-for-byte for consumers written against that release,
deprecated for accounting and never removed, renamed, retyped or redefined.
`earnings{coverage: exact|legacy|mixed|none|unavailable, exact, exactSince,
legacyStatus: none|estimated|unavailable}` qualifies the two accounting
lists; each sample carries `exact: true` when an exact event produced it. The
export endpoint carries the same fields over the exported series; on the
export every one of them is additive (the first release's export carried no
`breakdown`), so its `breakdown` is the same compatibility attribution over
the full-fidelity exported series, for parity with the history endpoint,
not a figure any older consumer could have read there. Exact and
estimated figures are **never summed**, and neither is ever folded into the
compatibility `breakdown`; a truncated raw series makes the legacy estimate
`unavailable` (never zero) while the exact aggregate stays complete. The
dashboard's accounting truth is `exactBreakdown` when exact data exists and
`legacyBreakdown` only as explicitly estimated data (every estimated figure
is marked `≈`, the legacy part on its own line); it never consumes the
compatibility `breakdown`. Retention prunes `point_events` with
`points`/`annotations`.

**One snapshot per response.** Both endpoints read everything they present
together — the balance samples with their `exact` flags, the annotations, the
exact aggregate and (history only) the settled bets — through
`Repository.PointsSnapshotBetween`, one read transaction on the shared
connection (`database.WithTx`). A point event committing during the request is
therefore in every component or in none: one read transaction yields one
consistent snapshot whatever the journal mode, the single pooled connection
cannot run the writer's statements until the transaction ends, and in the
default rollback-journal mode a writer on any other connection additionally
cannot commit against its SHARED lock (the observable the tests use). The
transaction takes no repository mutex, is committed before the method returns,
and nothing is held while the response is encoded. Every statement of the
snapshot runs under the request context, so a request abandoned by its client
interrupts the read it is on and releases the connection.

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

### Immutable Prediction Observations

An append-only trail of what the Prediction subsystem **saw** and **decided**,
stored in `prediction_observations` and `prediction_observation_sessions`
(analytics migration v6, see *Analytics Module Schema*). It answers "what did
the bot observe, and what did it do about it?" — a question neither
`prediction_bets` (settled bets only) nor `point_events` (awarded points only)
can answer, because neither records a decision that produced no bet.

The trail is a **pure observer**. It never places, modifies, gates or delays a
bet, never changes a Twitch call's count, arguments or result, and never feeds
the points ledger, the ROI report or any dashboard figure. Producers do only a
bounded copy and one nonblocking hand-off onto a capacity-512 private queue;
one collector goroutine performs every write, one row per transaction, under a
hard 5 ms deadline with no retry. A low-priority gate lets every analytics
write path preempt it: a business write cancels the single in-flight
observation transaction and waits only for it to settle. Any capture or write
failure — a full queue, a cancelled transaction, a disabled collector — is a
DROP recorded in the session's `dropped_count`, never an error a producer
could act on.

Nine closed kinds are recorded: `source_unknown` (a Prediction frame whose
shape could not be read), `channel_event` (a round lifecycle frame as it
arrived), `schedule_decision` (whether a new round was scheduled, and why
not), `auto_decision` (the automated due/decided/skipped outcome),
`manual_control` (an operator action's root and each phase it passed through),
`placement` (`CALL_STARTED` immediately before the single Twitch placement
call and `CALL_RETURNED` immediately after it), `user_prediction_made` (the
placement confirmation — outside terminal admission, exactly as the betting
code treats it), `user_terminal` (the terminal delivery and the admission
verdict the betting code already reached, read and never re-decided), and
`round_cleanup`. Exactly one manual root exists per operator action:
`MANUAL_MINER_ROOT` when the miner relays a dashboard action into a resolved
pool, `MANUAL_DIRECT_ROOT` for a direct pool call, never both. A nil-pool
failure never reached the subsystem and opens neither, so the absence of a
fact is not evidence that no attempt was made.

Only closed, typed, sanitized projections are stored or hashed. An
unrecognized value becomes `UNKNOWN` rather than being persisted verbatim, and
there is no free-text field at all — so raw PubSub or GraphQL bodies,
`Topic.String()`, the transport `EventFingerprint`, tokens, headers, Twitch
transaction identifiers, raw errors and predictor identities have no
representable path into a row. Outcome projections carry aggregate figures and
the *count* of top predictors examined; **zero** predictor identities are
retained.

#### The auto-decision envelope

The trail records what an automatic decision *was*, and — since producer
revision `obs-v2` — what it was computed *from*. An `auto_decision` fact now
carries a `decisionEnvelope` inside `payload_json`: the effective bet settings
the decision consumed (all nine fields, with a deep copy of the optional filter
condition), the ordered model outcome state read at `Calculate` entry
(identity, totals, `TopPoints`, and the derived `odds` / `oddsPercentage` /
`percentageUsers` exactly as the model held them), the points balance, the two
global stake gates the attempt was measured against (`maxStakePercent` and
`reservePoints` — whether the health gate was enabled is recoverable from the
recorded health state rather than stored as a setting), and the original
results of each stage. A `schedule_decision`
fact for an admitted round carries `admissionSettings`, the snapshot the round
was admitted with — recorded independently and never merged with, or asserted
equal to, the decision-time snapshot.

The envelope separates facts a single number would conflate: `choiceAmount` is
what the strategy proposed, `stakeAllowed` is what the stake gate returned,
`clampApplied` is whether the caller actually adopted that allowance (recorded
at the assignment, not derived from the gate reason), and `finalAmount` is the
stake carried out of the gate block — absent when the attempt returned from
inside it. Both results of the filter's single evaluation are recorded, so
"the filter ran and passed" is no longer indistinguishable from "the filter
never ran". A stage the attempt never reached carries an explicit
`NOT_REACHED`, never a zero: 0, `false` and `""` are all legitimate results of
a stage that *did* run. The health gate distinguishes four states — `DISABLED`,
`NO_GATE`, `ALLOWED`, `DENIED` — because "no gate ran" and "the gate allowed
it" are different facts. Every fact of one attempt, including both placement
calls, carries an `autoAttemptId` counter minted by the observer, so an attempt
is reassembled by a minted identity rather than by a timestamp or an event id a
re-admitted round would reuse.

Capture adds no business read. Every value is the one the existing path already
computed, retained at its original position under the lock that owns it and
deep-copied, so a later settings edit, outcome update or round replacement
cannot alter a fact already recorded. `Calculate`, `Skip`, the eligibility
check, the health gate and `EvaluateStake` are each still invoked exactly once,
in the same order. Stealth mode is observed, never reconstructed: the envelope
carries the complete inputs and the original post-`Calculate` stake, from which
the effective integer reduction is derivable for that exact input and choice —
it is not a captured `rand` draw, no seed is recorded or invented, and the
derivation authorizes no entropy reuse for any other outcome, stake or
strategy.

Outcome identifiers are stored verbatim. Unlike the routing identities (channel
id, event id, login), which are matched through a trimming comparison and are
therefore stored trimmed, an outcome id is never normalized anywhere else on the
path: the model keeps what the frame carried and the placement mutation sends
exactly those bytes. An id the sanitizer would have to alter — padded, or over
the frozen string ceiling only because of that padding — refuses the whole fact
and is counted as a drop, on the same rule that refuses a truncated one: a
changed identifier names a different outcome than the bet did, and storing it
would let a record be hashed and witnessed as complete while misnaming the
decision it claims to explain.

`TopPoints` deserves an explicit note, because it is the one value in the
envelope that describes an individual rather than a pool. It is the largest
single stake among a round's top predictors — one viewer's wager amount, not an
aggregate. It is stored because `SMART_MONEY` selects on it and stealth mode
reduces below it, so a decision that read it cannot be replayed without it. No
predictor identity is stored with it, and the wire projection alongside it still
keeps only a *count* of top predictors, never their contents.

`ObservationPayloadVersion` stays `1`: the envelope is an additive optional
field, a fact without one renders byte-identical JSON, and the constant is
hashed into every row's digest from its compile-time value while witness
verification re-reads the stored bytes — so bumping it would make every
historical row fail its own witness. `ObservationProducerRevision` moves to
`obs-v2` instead, which is the safe dial: a foreign revision is not an
integrity failure, it only tells a reader which contract's invariants apply. An
absent envelope in an `obs-v1` session is therefore a contract fact, never a
decision computed from nothing, and rows written under the old contract are
never backfilled. A ceiling breach or a non-finite number refuses the whole
fact and is counted as a drop, exactly as the outcome and predictor ceilings
already do — a decision record that silently lost an input is indistinguishable
from a complete one. The envelope's only identifier is the round-scoped outcome
id needed to link a choice to its placement call. Outcome titles and colours are
not projected, and beyond the `TopPoints` figure described above no predictor
data is retained.

#### Offline decision replay

`internal/predictioneval` reads the envelope back and re-derives the decision it
describes. It is an offline reader: it never places a bet, never changes a
stake, never proposes or selects a strategy, and has no runtime, scheduler or
HTTP surface. Its only product is a versioned, machine-readable comparison
between what a decision was recorded to have *read* and what it was recorded to
have *produced*.

The pipeline is four pure value-in/value-out functions —
`MaterializePairedKnowledge` → `ProjectDecisionCase` → `Evaluate` → `Score`.
Acquiring data is the separate job of `internal/predictioneval/reader`, the only
part that touches SQLite. The four core stages reach no database, network,
Twitch, PubSub, live setting, environment variable, wall clock or global RNG,
and a dependency-fence test enforces that over the package's whole transitive
import graph rather than by convention. The fence pins that graph EXACTLY rather
than screening it against a list of packages someone thought to name: any
reachable package that is not pinned fails, so a capability nobody anticipated
cannot arrive unnoticed. A Go toolchain upgrade that changes what those imports
drag in is expected to trip it, which is the point — that change is reviewed,
not absorbed.

**Causal separation.** The producer persists an attempt's inputs and its results
in one terminal envelope, so the reader performs the split the writer could not.
`MaterializePairedKnowledge` groups facts by the minted `autoAttemptId` — together
with the pool instance and collector session, because the attempt counter
restarts with the process — and bounds each attempt's *common-input slice*: the
causally-closed prefix up to and including its terminal fact. Both placement
facts carry the same attempt id and fall **after** that boundary, so they reach
`Score` alone and can never feed the decision that produced them. The slice is
digested before any model projection, so appending later facts to the store
cannot change an earlier case or its digest. `Evaluate` receives only
`DecisionInputs`; a recorded choice, a later outcome, a post-placement state and
a settlement are not merely unused there, they are unrepresentable in its
signature.

**Faithful baseline, not a substitute strategy.** The model reconstructs the
policy each decision was actually configured with — every strategy the pinned
policy dispatches on, its strict-`>` tie handling that keeps the lowest index,
`SMART`'s strict-`<` gap comparison, the `NUMBER_*` fallback to slot 0, the
`decision_*` → `total_*` key remapping, the int truncation in the stake, and the
caller's `Calculate → Skip → health → stake → clamp → minimum → filter` ordering,
in which the filter is *computed* early and *acted on* last. Derived odds and
percentages are read from the envelope as the model held them, never recomputed
from newer wire totals. `EvaluateStake`'s returned allowance and the amount the
caller actually adopted stay distinct facts, and a reserve violation produces no
post-gate stake because the pinned caller returns from inside the gate block.
Where an unrecognised strategy, filter key or comparison operator was stored as
`UNKNOWN`, the replay is still exact: the pinned switches have no `default`, so
every value outside a closed set takes the same branch.

**What cannot be proven is not claimed.** Stealth mode consumes a random draw
that was never recorded. The model establishes the choice, the base stake and
whether stealth applies *without* any observed value, and only then uses the
recorded pre-risk stake to pin down which of the four legal integer reductions
occurred. That stage is reported as `CONDITIONED_ON_OBSERVED_REALIZATION`,
everything derived from it is counted apart from independent evidence, and the
comparison of the reconstructed stake against the stake it was conditioned on is
labelled circular rather than counted as a passed oracle. A realization that is
missing or unreachable by any legal reduction stays `UNPROVEN`; it never becomes
the base stake. Eligibility and health verdicts are echoed as witnessed external
inputs — the replay cannot see the transport health or round state behind them,
and agreement about them proves nothing about that hidden state.

**Refusals.** The reader binds to `payload_version` 1 and producer revision
`obs-v2|policy-378d05d6ccc7d2a914730a1e1d023ff754bcf873`; the replay model and
its results carry their own version and the pinned policy revision, which are
never restamped with the SHA of whatever build is replaying.

The revision BINDS rather than merely annotating. A session stamped with a
revision this model does not know yields no cases at all: an unknown contract
may have changed what a field means, and a confident verdict against a contract
nobody re-read is worse than no verdict. `obs-v1` is the one known exception —
it stays readable and acquires nothing, because a missing envelope is a contract
fact there, and no default, current setting or neighbouring fact is used to
manufacture one. A pre-envelope auto fact carries no attempt discriminator
either (the counter and the envelope shipped in the same commit), so it is
refused by name as a legacy-contract fact rather than as a fact whose attempt
never began.

The strength of the integrity check has a bound, and the replay states it
rather than trading on it: the store's row witness is an UNKEYED SHA-256 stored
beside the data it covers. It detects accidental corruption — a torn write, a
bad page, a partial restore — and it does not authenticate against an adversary
who can write to the database file, who can recompute the witness after editing
a payload and repair the session counters to match. The common-input digest
inherits exactly that property and adds no authenticity of its own; what it does
hold against a hostile store is narrower and still useful, namely that a case,
an evaluation and a settlement cannot be recombined across attempts. Detecting
malicious edits would need a MAC or signature keyed outside the database, which
is a producer change.

Session integrity, truncation and witness verification are consumed from
`ReadObservationSession` rather than re-implemented — the reader-facing
projection COALESCEs NULL parent ids and re-marshals the payload, so it could
not reproduce the digest's inputs even from copied code. A session the store
calls `INTEGRITY_ERROR` yields no cases, and so does one written under an
unsupported revision; an unfinalized or truncated session yields cases carrying
that qualification, and the qualification travels ON the case — a scorecard
names the session, the witness counts and the loss counters it was read under,
so a result from a truncated, unwitnessed session is never mistaken for one from
a fully verified run.

**A dataset must describe itself.** `MaterializePairedKnowledge` takes a VALUE,
so a caller can slice a dataset while keeping the classification that described
the whole of it — and the classification is what every downstream stage trusts.
Dropping one of two terminal facts is the sharp case: an attempt that must be
excluded as `MULTIPLE_TERMINAL_FACTS` becomes an ordinary materializable case,
and the scorecards drawn from it carry a clean `AS_FINALIZED` provenance
describing facts the dataset does not contain. So the count of facts the session
OWNS is compared with `FactsPresent`, and a mismatch yields no cases. It is
counted over owned facts because a dataset may legitimately carry another
session's rows, which were never part of that count. A real load always
satisfies this: the reader refuses rather than truncating, and keeps an
undecodable payload as a row rather than dropping it.

**A placement fact's status is one fact, not two.** The producer computes a
placement call's reason code and its error class from ONE error value: a nil
error yields `OK` beside `NONE`, and a non-nil error yields a rejection beside a
class naming it. Reading acceptance from the reason alone let a record claim
both — accepted, and carrying a `TRANSPORT` or `INTERNAL` class — pass every
attribution check and reach an affirmative settlement while its own facts
reported the call had failed. The pairing is validated on both placement facts
before the shape is called coherent.

**A refusal is not a claim that nothing happened.** `NOT_APPLICABLE` asserts
that the replayed decision reached no placement. `LEGACY_FAILURE`,
`INDETERMINATE` and `UNSUPPORTED` establish no such thing — they say the model
could not get far enough to know what the decision would have done. Those three
yield `UNKNOWN`; only a determined skip, where the model followed the policy to
an exit, yields `NOT_APPLICABLE`.

**Bounded acquisition.** A load is bounded at four levels — the session row,
the facts the witness sweep reads, the row count and the bytes — plus a
preflight count that bounds the work of getting there. Every byte bound is
enforced before the data it bounds is materialized.

The SESSION ROW is measured first, because it is read first, and — like the
fact row — **every** column it selects is measured.
`prediction_observation_sessions` is not `STRICT` either, so its nine
INTEGER-affinity columns hold arbitrarily large TEXT as readily as its three
TEXT ones. The `CHECK (col >= 0)` constraints on five of them close nothing:
SQLite ranks the TEXT storage class above INTEGER, so a TEXT value compared
against the integer literal 0 with `>=` passes whatever it contains — measured,
a 250 KB string inserts into such a column and reports `typeof() = "text"`.
Bounding the facts while scanning this row unguarded would leave the earliest
allocation of the whole load the only unbounded one (`MaxSessionMetaBytes`,
64 KiB). One structural test covers every `SELECT`/width pair, so none can
drift from the columns it is meant to measure.

The WITNESS SWEEP is the read that hid behind the other three.
`ReadObservationSession` is not only a session-row read: inside the same
transaction it recomputes the stored witness of a bounded prefix of the
session's facts, and doing so scans `payload_json` and a dozen identifier
columns of each row into memory one row at a time. Every other byte bound is
keyed on the SESSION ID, which the load learns *from that call* — so they all
applied strictly after those rows had been materialized. Measured on a
`payload_json` tampered to 4 MiB, four times `MaxRecordBytes`: the sweep
scanned and hashed it in full, and the load then returned no facts and no
error, having already paid for it. `MaxRecordBytes` is therefore applied to the
sweep as well, inside the SAME TRANSACTION, measured over exactly the rows the
sweep would touch — the same key, the same ordering, the same budget. A bound
narrower than that leaves rows unmeasured; a wider one refuses loads for rows
the sweep never reads.

The EPOCH bound is the fourth level and bounds WORK, not bytes. The session
read classifies by aggregating over the session's facts before recomputing any
witness, so a store that assigns millions of rows to one epoch spends unbounded
database CPU and I/O inside that call before any row limit could refuse the
load. Counting a bounded window of `limit+1` rows first makes the refusal cost
the size of the answer, and a count that REACHES the window is itself the
refusal: it was truncated, so it describes a prefix rather than the epoch. An
earlier version measured the widest row here too; that was both over-broad (it
covered rows the sweep never reads) and stale (a separate statement leaves a
window a writer can commit into), and the width now travels with the sweep.

Every byte bound travels IN the statement — or, for the witness sweep, the
transaction — that reads what it bounds,
and that co-location is the property rather than an optimization. A width
measured by an earlier, separate query is a time-of-check/time-of-use gap:
another connection commits into it, and the read then transfers the enlarged
value across the driver having never been covered by any bound. The session row
is read TWICE — once for the classification, once to prove the snapshot did not
span two committed states — and BOTH readings carry the bound, because an
unbounded re-read still reports that the store changed, by scanning the row it
should have refused. Tests observe the bound travelling with each read rather
than inferring it from the verdict, since the verdict is identical either way.

The epoch COUNT is a preflight rather than a co-located bound, and it is stated
as one: it bounds work, and a store that adds rows between that count and the
read is caught by the coherence re-read rather than prevented. No byte bound
rests on it.

The ORPHAN CHECK is the one scan no caller-side preflight can cover, and it is
bounded by the SHAPE of its question rather than by a limit. Inside the
classification, `ReadObservationSession` asks whether any fact matches exactly
ONE half of the `(epoch, session id)` pair. The epoch count covers the disjunct
keyed on the epoch; the disjunct keyed on the session id reaches rows under
OTHER epochs, which that count never saw. Asked as `COUNT(*)` this walks every
one of them to produce a number the classification does not read — measured at
500,000 such rows: 5,000,029 VM steps against 24 for the preflight.

Asked as `EXISTS` it stops at the first match, and that bounds both directions
without weakening anything. Either a row matches, so the scan ends there, or
none does — which means every row carrying this session id also carries this
epoch, and the epoch count already refused the load if there were more of those
than the limit. A `LIMIT N` would have been the wrong instrument for exactly the
reason a limit is wrong here: "no orphan found within N rows" is not "no
orphan", and buying a cost bound by weakening an integrity check is not a trade
this reader makes. The bound is pinned structurally, because a count and a
presence flag are indistinguishable in the result.

The byte aggregates run over a BOUNDED candidate set of `limit+1` rows, not the
whole session. Measuring every row first would let a store holding millions of
small rows spend unbounded database CPU and I/O to produce a number whose only
use was to refuse the load — a bounded allocation reached by unbounded work.
`limit+1` keeps a session AT the bound distinguishable from one over it. The row count is capped
(`DefaultMaxRecords` 20000, ceiling `MaxLoadLimit` 2^20 — a bound large enough
to overflow the `limit+1` probe is not a bound and is refused), and the
AGGREGATE width of the session is capped (`MaxSessionPayloadBytes`, 128 MiB)
alongside the width of any single row (`MaxRecordBytes`, 1 MiB), by
`COUNT`/`SUM`/`MAX` aggregates the database evaluates without handing any value
across the driver boundary. The byte bound is not redundant with the row bound:
`MaxObservationPayloadBytes` is enforced by the WRITER, so it bounds a store
the writer filled and bounds nothing in a tampered or foreign database file —
and even where it holds, 20000 facts at 64 KiB is 1.25 GiB. A session over any
bound is refused, never truncated, because a prefix is indistinguishable from a
complete dataset to every stage downstream.

Two details of that bound are load-bearing. The measured width is **every**
column the read materializes — not `payload_json` alone, and not "the
variable-width ones" either. `prediction_observations` is not a `STRICT` table,
and outside a `STRICT` table SQLite treats a declared type as an affinity rather
than a constraint: an INTEGER-affinity column such as `received_at_ms` holds an
arbitrarily large TEXT or BLOB. Measured directly, a 300 KB value stored there
reports `typeof() = "text"` and a 300 000-byte width while an expression
covering only the nominally variable-width columns reports 2. Affinity is
therefore not consulted at all, and a structural test compares the width
expression against the `SELECT` list itself, admitting no exemptions and failing
when either grows a column the other lacks. And
the bounds are restated INSIDE the reading statement rather than inherited from
the earlier measurement, because two statements are two snapshots: a value
enlarged in between would leave the row count unchanged, pass a count re-check,
and be materialized having never been bounded. The per-row cap exists for what
an aggregate cannot cover — a read aborted on exceeding a running total has
already materialized the row that exceeded it. A session the store already
classified `INTEGRITY_ERROR` is refused before the rows are read at all: it was
going to yield no cases, so there is nothing to gain by paying for its content.

Neither a `COMPLETE` session nor a verified digest is ever treated as proof that
an individual decision case is complete: the envelope's own stage states are
checked against the producer's structural invariants, and a snapshot that breaks
one is refused rather than read under assumptions that do not hold for it. A
missing or unusable input makes its stage `UNSUPPORTED` or `INDETERMINATE` —
never `0`, `false`, `SMART` or "skip" — and a stake the pinned policy's `int`
arithmetic could not represent or would wrap is reported explicitly instead of
being silently truncated.

A round is qualified by its capture ORIGIN, not by the presence of a gap cause.
Both capture columns are nullable and the schema admits `UNKNOWN` and
`PREFIX_UNOBSERVED_AT_ADMISSION` with no cause, so anything but
`ACTIVE_AT_ADMISSION` — including an absent origin — reports
`ROUND_ADMITTED_WITH_INCOMPLETE_CAPTURE` rather than passing as fully captured.

**Attribution, not coincidence.** An affirmative settlement requires the
recorded placement to be *this* attempt's. Three things are checked and none of
them alone is enough. The placement facts must have the SHAPE the producer
writes — exactly one `CALL_STARTED` followed by exactly one `CALL_RETURNED`,
both carrying the stake and outcome slot the producer puts on both, and both
carrying the same ones; anything else is `INCOHERENT` and settles nothing,
because a stake read from one call beside an acceptance read from another is
not one observed call. Those arguments must be the ones the replay derived. And
the facts must be stamped by `ProjectSettlementFacts` with the attempt key and
common-input digest of the case being scored, because a stake and a two-option
slot are low-cardinality enough that a different attempt on the same round can
carry the same pair by coincidence — matching arguments are evidence, not
identity.

A terminal action is also a claim about which stages ran, and the claim is
checked in full: `WOULD_ATTEMPT_PLACEMENT` requires the choice, base stake,
filter, stake gate, clamp and minimum stages each to be `EXECUTED`, the health
gate to be `WITNESSED`, and a final amount to be present. Every stage, not the
conspicuous ones — omitting the stake gate would leave the risk-gate result that
determines the final stake uncompared. The required shape is derived from what
real evaluations produce rather than restated, so the guard and the evaluator
cannot drift apart. A partially decoded or caller-edited evaluation can carry
that action with those stages blank, and the per-stage comparisons are then not
`UNAVAILABLE` — they are **absent**, so the unavailable-evidence guard below
sees nothing to object to.

No comparison may be UNAVAILABLE either. A comparison that could not be made is
evidence that is *missing*, and it is not a disagreement — so counting only
disagreements let a case with an unrecorded field carry an accepted placement to
an affirmative settlement. An affirmative assessment asserts that the recorded
settlement describes the replayed decision, and that claim cannot rest on a
field nobody could check.

Every fact of an attempt must describe ONE admission of the round, on both
sides of the causal cut. Checking only the input prefix left the half that feeds
the settlement unguarded: a placement fact carrying this attempt's counter but a
different `round_incarnation_id` reached the settlement projection and could
supply the stake and slot for a different admission.

The recorded terminal fact must also NAME its action. The producer writes
`PLACE` beside `AUTO_DECIDED` and `SKIP` beside `AUTO_SKIPPED` on every terminal
auto fact it emits, so a blank decision is a record it cannot have written: the
case is refused as `INCOMPLETE_TERMINAL_RECORD`, and the phase and decision are
compared unconditionally rather than skipped when absent — a value that produces
no comparison agrees with every replayed action.

How a round SETTLED is not attributable to an attempt at this producer revision.
The discriminator that links an attempt's facts is absent from the
`user_terminal` fact carrying the win/loss verdict and the payout, and a round
can carry more than one attempt — so joining on the round would credit one
attempt with another's payout. Resolution, payout and returned stake are
therefore reported `UNKNOWN` rather than guessed. Making them attributable is a
producer change, not a reader change.

`WOULD_ATTEMPT_PLACEMENT` is a statement about the policy reaching the placement
call, not a claim that a bet was placed or accepted. Settlement facts reach
`Score` only after `Evaluate` has run, and describe the *replayed* decision only
when the replay independently reproduced the recorded one; otherwise they are
`UNKNOWN`. Profitability, ROI and bankroll effects are not computed at all.

The module is proven against controlled datasets written through the real store
and read back through the real reader, and against real decisions driven through
the real pool. It has **not** been validated against a production observation
dataset; collection and empirical replay are separate work.

#### Ordered-rules reference core (supplied data)

`internal/predictioneval` carries a SECOND, separate pure model beside the
baseline replay: a re-derivation of a donor project's ordered odds-rules
mechanism — ordered detailed rules with a per-rule participation rate, a
per-outcome default, and a percentage stake. Two exported functions,
`ProjectOrderedRulesStream` and `EvaluateOrderedRules`, are the whole surface.
There is no collector, no schema change, no reader, no HTTP or settings
contract, no runtime caller and no new dependency; the package's existing
dependency fence and its allowlist are unchanged.

The two models do not meet. The ordered-rules core reads no persisted fact,
feeds nothing back into the four seams and cannot change a baseline result. It
carries its own versions — `predictioneval-orderedrules/v1`, the stream contract
`pe-ors/v1`, the raw-config basis `pe-orc-raw/v1` and the entropy semantics
`rand-0.8.5-bernoulli/v1` — none of which is `predictioneval/v1` or `pe-cid/v1`,
because attaching a claim about verified database rows to numbers passed in as
arguments is exactly the confusion these versions exist to prevent.

**Supplied is not proven.** Every value reaches the model because a caller
passed it in, and a pure function cannot authenticate a number it is handed. So
presence is a first-class part of the input types rather than a nil pointer
someone can read as a zero, provenance travels with each field, and every result
carries the label `CORE_MECHANISM_COMMON_ADMITTED_DATA` — the mechanism,
evaluated over the declared common-admitted data, and nothing more. The
admission manifest is DATA, not a callback: admission is fixed before evaluation
and cannot be chosen for producing a better answer.

That rule covers the MANDATORY scalars too, and it has to, because the input
types carry JSON tags: decoding a caller's document is a supported way to build
them, and there an omitted field is silent. A candidate's causal position, an
intervention's position, the scope's interval endpoints and the config's default
each decode to a zero that is ALSO a legitimate value, so each carries an
explicit declaration that it was supplied and is refused without one. The cost
of leaving any of them implicit is not abstract: an omitted candidate position
decodes to zero, is admitted as the EARLIEST candidate whenever the declared
interval contains zero, and takes the opportunity from whichever candidate
really was first; an omitted intervention position cuts the stream at zero and
removes every candidate; omitted endpoints decode to the interval [0,0], which
is a real interval; and an omitted default decodes to a USABLE [0,0] rule that
admits any zero-share outcome and reports a stake of zero with presence KNOWN —
a fabricated value presented as known, from a configuration that was never
supplied. Treating all-zero as absent would not fix it, because the donor's own
`small` preset really does ship bounds of exactly zero.

The outcome VECTOR carries a presence of its own, not only its scalars. A pool
holding fewer than two outcomes is a pool the donor declines and the traversal
walks on; a pool whose vector the caller could not recover looks identical — a
short slice — and is the opposite fact, because walking past it hands every
later candidate an opportunity that exists only because this one was skipped. A
short vector is the donor's decline only when the caller vouches for it as
whole; anything else stops the traversal. A partially recovered vector must be
declared invalid rather than passed off as complete, since the pool total feeds
every share and one missing entry moves all of them.

The same rule reaches the AVAILABILITY of a supplied value. A KNOWN value
carries the causal position at which it became knowable, so that back-dating is
checkable rather than trusted — a value that only became available AFTER the
candidate it is attached to is refused outright, since a note would not stop the
arithmetic from using it. That position is an integer whose zero is a legitimate
position, so it is accompanied by its own declaration flag: an omitted position
decoded as a zero would otherwise clear the check for every candidate at
position zero or later, which is the whole interval of any stream starting at
zero, and the guard would read as enforced while enforcing nothing. A KNOWN
value that does not declare its availability is refused, exactly as one carrying
no provenance is.

Supplied COLLECTIONS are bounded by count before their bytes are charged —
candidates, outcomes, interventions, rules, entropy words and the admission
manifest's source references alike. The aggregate byte budget charges payload,
and payload is not a bound on cardinality: elements with no payload cost nothing
to admit and still have to be retained, copied and digested.

Those bounds are checked BEFORE the work they bound. The evaluator is exported
and takes the projected stream by value, so it can be handed one the projection
never produced; it already refuses such a stream on a digest mismatch, but that
comparison is circular, because detecting a forgery requires digesting the
forgery first. What can be bounded is the COST of a forgery, so a shape gate
runs ahead of every digest and every allocation, using counts and string lengths
only — each of them a single length read over a number of elements the preceding
check has already bounded. It mirrors the projection's own limits field for
field, the per-string limit included, so an input the projection would have
admitted is not refused there and one it would have refused is not hashed there.
The aggregate itself bounds the ENCODED width of that text, not its raw length,
and the two differ by up to six: the stream carries JSON tags because it is
meant to be serialized, and the encoder writes a byte below 0x20, one of < > &,
or anything that is not valid UTF-8 as a six-byte escape. Bounding the raw total
bounded the wrong quantity — a source could sit inside the declared 128 MiB and
still encode to roughly 768 MiB — so the projection charges every supplied byte
at its worst-case encoded width. That is a deliberate over-charge for ordinary
ASCII, taken because computing the exact width means decoding UTF-8 and the
production files import from a six-entry allowlist with no unicode/utf8 in it; a
bound that is provably never exceeded is worth more in budget code than a tight
one. Charging the text is still not the whole ceiling, because JSON syntax —
the quotes around each string, the field names beside it, the braces and commas
holding the document together — is charged to nobody and lands on top of a
budget the caller has already filled: measured at the widest shape the counts
allow, just over a megabyte of pure structure, which put an ADMITTED stream
762,930 bytes past the ceiling it was supposed to sit inside. So a fixed reserve
is held back from the aggregate for it, sized from that measurement and re-taken
by a test rather than assumed, since assuming it is what went wrong. The
per-string limit is what keeps the reserve sufficient: without it a single
unbounded field could fill the remaining charge byte-exactly and leave the
structure to land past the ceiling, and four scope strings and three admission
strings really were unbounded — checked for being non-empty and nothing more —
so an 8 MiB admission population was admitted outright. The evaluator's shape gate
applies the SAME charge and the same reserve, and for a while it did not: it
compared the RAW total, and the two differ by a factor of six. That gap was not
one-sided safety. A forged stream carrying 32 MiB of perfectly valid
provenance — inside every count, inside every per-string bound, inside the raw
aggregate — passed the gate and every semantic invariant while the projection
refused that same source outright, and a stream the gate admits can still be
serialized, which is the cost the charged width exists to bound. The per-string
limit went the same way: the seven strings above gained a bound in the
projection before the gate had one, so a namespace just under the aggregate was
hashed by all three whole-input digests before the invariant pass refused it —
646 ms and 251 MB allocated to say no, which is precisely the length-before-work
protection the gate exists to provide. Both sides now apply the same condition,
so "admitted by the projection" implies "admitted here" by construction rather
than by a margin.

Mirroring means the mirror is exact in both directions: text the projection
DERIVES rather than receives — the qualifications it generates, the boundary's
kind and basis, the stream's own contract version — is length-bounded but never
charged, because the projection never charged it, and the config and the trace
carry a ceiling of their own, because the projection never saw them at all. Two
inputs, two ceilings, and the derived fields on neither; otherwise a source
filling the projection's budget would project successfully and then be refused
here for bytes, which is the invariant backwards.

The boundary's IDENTITY is the exception, and calling the whole boundary derived
was wrong. Kind and basis are labels the projection computes; the identity it
COPIES from a supplied intervention identity, and charges those bytes against
the source budget. Excluding it from the ingest charge let a forged stream carry
text no source could have supplied — a stream declaring an established cutoff
had at least the one intervention whose identity it names. Charging it stays
inside the projection's own total, since the projection charged every
intervention, so the mirror tightens without the invariant moving.

The boundary's REMOVAL COUNT is the same argument one step further. A stream
declaring that the boundary removed candidates asserts a source holding that
many more of them, and the projection charged every one: its validate-and-charge
loop runs before the cut, so a removed candidate cost it exactly what a retained
one cost. The ingest gate cannot see that text, but it does not need to, because
the vocabulary the projection forces puts a floor under it — the cheapest
candidate it will admit spends a one-byte identity, the only accepted
membership, the shortest outcome-vector presence and a KNOWN balance with the
single provenance byte that then becomes mandatory, plus the source kind. That
last part is per VIEW rather than per model, and treating it as though it were
not undercharged one of the two: the projection couples view and source kind
strictly, so a CALCULATE_ONLY stream's cheapest candidate is 36 bytes
(CALCULATE_SNAPSHOT) and a CHANNEL_CANDIDATE_STREAM's is 32 (CHANNEL_UPDATE),
216 and 192 charged. Because the coupling is strict, each is the EXACT cost in
its view and not merely a bound; charging the cheaper of the two everywhere let
a forged calculate-only stream sit 24 charged bytes a removal past what any
source could have projected. Leaving the claim free let a forgery sit within that much
per claimed removal of the ceiling and pass a gate whose whole purpose is to
refuse what the projection refuses. The floor is a LOWER bound by construction,
which is the direction that keeps the mirror one-sided: charging less than the
projection charged can only admit streams it would also have admitted. The count
is read only when positive and is clamped to the candidate ceiling before the
multiplication, because this gate runs ahead of the invariant pass that bounds
it — unclamped, a negative count would subtract from the budget and a count near
the integer maximum would overflow into one, so the reserve would become a
discount.

Two version comparisons — the stream's contract version and the entropy
semantics version — are decided BEFORE the whole-input digests, for the reason
the gate exists at all. Each is one string against a constant and each settles
that the input is not evaluable; computing three digests first does the work the
check governs. Measured with a 125 MiB config identifier beside a wrong contract
version, that refusal cost 1.23 seconds and 251,662,352 bytes to compare two
short strings. Continuing:
the per-string limit is not implied by the aggregate, since one 64 MiB note sits
well inside a 128 MiB budget while being a value the projection refuses outright.
Re-establishing those invariants means ALL of them, and the mandatory
declarations are the easy ones to forget: enforcing a candidate's declared
position only in the projection left the ingest path comparing a position the
caller never declared, so the projection refused an input the evaluator admitted
— reached over the same two-call digest oracle, since a refusal hands back the
value it wanted. A declaration that gates admission is exactly the kind of field
worth stripping from a forged stream, so it is checked here as well as there,
and before the comparisons that read the value it declares.

A refusal from that gate carries no whole-input digest: the model declined to
read the input, so it attests to nothing about it, and for the same reason the
gate outranks the contract and digest mismatches — an input too large to read
cannot be checked for anything else. It re-exports none of the supplied TEXT
either, and that is the same rule rather than a second one. The gate stops at
the FIRST condition that trips and most of them never look at the boundary, so
the cutoff's three text fields can reach the refusal having passed no per-string
bound at all; carrying them back out would move the cost from the gate to
whoever encodes the result — which the JSON tags say is the intended use — and a
refusal decided in constant time would still write a gigabyte of supplied text,
six-fold once JSON escaping is counted. Refusing cheaply and RETURNING cheaply
are one guarantee, not two.

The selection digest detects CHANGE, never ORIGIN, and the difference decides
what has to happen on ingest. The digest is unkeyed, deterministic and computed
over exported fields, so any caller able to construct the stream can compute the
matching value; worse, a refusal returns the recomputed digest, so copying it
back takes one extra call and no cryptography. None of that is fixed by hiding
the value or the algorithm — the stream carries JSON tags because a projected
one is MEANT to be serialized and read back, and a check a legitimate reader can
repeat is a check anyone can repeat. So a matching digest is read as what it is,
evidence that the stream has not changed since the digest was taken, and the
evaluator re-establishes the projection's invariants over the stream it is
handed before traversing it: scope and admission completeness, identity,
uniqueness, strict causal order, the declared interval, proven membership,
source/view compatibility, and the presence and availability of every supplied
value. Those checks are the projection's own, called rather than restated, so
the two cannot drift.

The DERIVED fields need a different treatment, because the stream no longer
carries what they were derived from. A boundary is computed from interventions
the projection did not retain, so a caller handing one in is asserting a fact
that can no longer be recomputed; what remains checkable is that the assertion
is internally possible — an unestablished boundary names no intervention and
removed nothing, an established one carries a real intervention kind, a
non-empty identity and a position inside the declared interval, and a removal
count is never negative. The qualifications are stronger than that: they follow
deterministically from coverage, boundary basis, view kind and removal count, so
they are re-derived and compared as a whole. Equality, not containment — a
missing limitation would let a result read as though the absence of an earlier
intervention were established, and an added one asserts a limitation the
evidence does not support, both silently, because qualifications travel verbatim
into every result computed from the stream.

**The mechanism.** The outcome vector is the OUTER loop and the rule list the
inner one. A pool share is `1/(total/points)` — two divisions, never collapsed
into `points/total`, because in binary64 they differ: for the pool `[9,1]` the
donor's value is one ulp below the double a raw threshold of 90 normalizes to,
so a `Ge 90` rule separates the two formulations. Comparators are inclusive; a
matched comparator whose participation draw FAILS falls through to later
overlapping rules rather than ending the scan; and when no rule admits, the
CURRENT outcome's default is checked — inclusive on both bounds, never reordered
when the minimum exceeds the maximum — before the next outcome's rules. Raw
percentages in 0..100 are divided by one hundred exactly once, privately: the
exported contract accepts the raw form only, so an already-normalized config
cannot be normalized again. Stakes are sized as the donor sizes them — multiply
in float64, truncate to `u32`, then cap, with a cap of zero meaning NO cap — and
a computed stake of zero stays an attempt rather than becoming a skip.

**Entropy is supplied, never generated.** The donor drew from a thread-local
generator whose realization was never recorded, so the historical entropy is
UNAVAILABLE and is not reconstructed. What is reproduced exactly is the
CONSUMPTION rule pinned from `rand` 0.8.5: a participation rate of exactly one
succeeds consuming NO word, a rate of exactly zero consumes one word and then
fails, and anything between compares `raw < uint64(rate * 2^64)` strictly. A
comparator that did not match never reaches the draw, and a default admission
adds none. Words are consumed in order across the whole run and never restart.
An exhausted trace is an explicit unknown — never a default draw, never a skip.
The baseline's `ObservedRealization` is structurally excluded: it is a recorded
draw from a different mechanism, conditioned on a decision that already happened.

**Identity conflicts are refused, never reconciled.** A candidate identity is
unique within a source and an intervention identity within the boundary scan,
because two things carrying one identity are an input conflict where neither
dropping one nor keeping both is a safe reading. Outcome identities were the
exception until a reviewer noticed, so a pool could name the same outcome twice
while the model computed shares over it and reached a decision — a pool that
cannot exist in the source domain. They are unique within their candidate now,
on both the projection and the ingest path. The selection was never ambiguous,
since it carries the outcome INDEX beside the identity; what was wrong is that
the model answered at all.

**The factual boundary.** A stream is bounded by the first relevant placement
call in the declared episode, automatic or manual. Manual calls carry no attempt
discriminator and may carry a different or empty round incarnation, so they are
supplied as first-class interventions rather than discovered by attempt
grouping — grouping is what would miss them. A call that FAILED is still a
boundary, and the intervention type has no field for a result at all. Where an
association cannot be established, the earlier, conservative boundary is taken
and labelled rather than skipped. Coverage must be declared: an unstated
coverage is refused, because it makes "no intervention was supplied"
indistinguishable from "the intervention was never collected". A declared gap is
evaluable and its qualification travels into every result.

The boundary travels onto the result, not only the stream. Without it a
traversal that found nothing because the boundary removed EVERY candidate would
be field-for-field identical — counters and consumed-prefix digest included — to
one over a source that never held any, and those are opposite pieces of
evidence. A boundary that excluded supplied candidates is reported with its
count.

**Stops, and what they are not.** The traversal ends at the earliest of: the
first admission with a computable stake; the first admission whose stake is not
computable; the boundary; a required input it reached and could not read; or a
resource bound. After an admission it stops rather than continuing — there is no
counterfactual placement success, retry, pool mutation or balance update, none
of which is established by anything this repository persists. A stop with an
unknown stake is neither a zero nor an abstention: the model knows WHICH outcome
it would have bet and does not know HOW MUCH, and those are reported as separate
fields. A missing balance is never converted to zero and never borrowed from
another candidate. `NO_ATTEMPT_IN_SUPPLIED_PREFIX` is a statement about the
supplied prefix alone — not a full-round skip and not a financial zero.

**Bounds and bindings.** Offline resource limits — 128 candidates, 64 outcomes,
128 rules, 4 KiB per identifier and per retained free-text string — a presence
reason, a provenance note, a coverage detail — 2^20 draw words and 2^18
predicate slots — are
refusal boundaries, never truncation boundaries: an oversized input is rejected
whole, because a silently shortened candidate list changes which opportunities
exist. The slot ceiling is the tightest of them for a reason: every evaluated
slot also appends one trace entry, so the ceiling and the retained trace are the
same quantity, and it is set where that trace still fits the declared 128 MiB
aggregate budget rather than where a slot count alone would allow. That budget
binds under BOTH measures, because the result carries JSON tags: in memory two
identifiers are two headers pointing at bytes the stream already owns, but
encoded, every entry writes them out in full — so repeating a candidate and an
outcome identity on each slot turns a 22 MiB retained trace into 2 GiB of JSON,
before escaping, from an input inside every other declared bound. A trace entry
therefore ADDRESSES its slot rather than naming it, by the candidate and outcome
indices the stream digest already binds, and its encoded size does not move with
the caller's identifier lengths at all. Four domain-separated digests bind a result to its inputs: the whole
stream, the raw config, the whole supplied entropy, and — separately — only the
prefix actually consumed. That last one deliberately excludes metadata about the
full supplied set, so that appending facts beyond the boundary provably cannot
move it. What it must NOT exclude is any mandatory declaration the source could
not exist without, and for a while it excluded three of them: the scope's
association evidence and the admission's population and order basis. Binding the
account context without the association evidence bound the claim and not its
warrant, so two prefixes read under different evidence — one solid, one only
just admissible — carried the same consumed-prefix binding, and a result
computed under either could be presented as a result computed under the other.
That is the recombination these four exist to prevent. The declared interval's
UPPER endpoint is the one mandatory declaration still excluded, and
deliberately: it is the source's extent, and appending a fact past the boundary
can legitimately widen it, so binding it would break the stability the digest is
for. Its LOWER endpoint looks like half of the same value and is not — no append
lowers it, so binding it costs that stability nothing, and it carries evidence
the upper endpoint does not. Two sources holding the same candidate and no
intervention, one declaring complete coverage over [0,100] and the other over
[-100,100], read the same prefix; only the second also asserts that nothing
intervened over the hundred positions before it, which is a stronger claim about
the absence of an earlier intervention. It is bound; the pair is asserted in
both directions, so binding the upper endpoint as well fails the suite rather
than passing as a tidier-looking symmetry. Like the
baseline's, these are unkeyed hashes over supplied data: they prevent
recombination and authenticate nothing.

**What this is not.** It is NOT a faithful replay of the donor's full runtime
policy, and no result may be described as one. The donor branches on lock/end
timestamps, refreshes its balance against a live API before each attempt,
re-enters on every round update until a placement succeeds and may retry after a
failure; none of that is established by anything persisted here, so none is
modelled. It says nothing about profitability or about whether either mechanism
is better. Automatic extraction of a full donor-candidate stream from the
existing rows is NOT possible — the wire projection already lost distinctions a
reader cannot restore, such as an outcome holding zero points against one whose
points were never recorded — and remains separate work. The donor is Apache-2.0
and this repository GPL-3.0, a permitted direction of inclusion; no donor source
bytes were copied, and the pins, blob hashes and licence disposition are recorded
in `internal/predictioneval/testdata/ordered_rules/PROVENANCE.md`.

### Event Types for Series

Reasons tagged on balance-timeline samples (`points.event_type`, display form
with underscores replaced by spaces). They label the chart tooltip and the
legacy estimate; the exact ledger stores the raw Twitch `reason_code`.

| Event | Description |
|-------|-------------|
| `Watch` | Points from watching |
| `Claim` | Points from bonus claim |
| `Watch Streak` | Watch streak bonus |
| `Raid` | Raid participation |
| `Prediction` | Prediction result |
| `Spent` | Points spent (never an earning) |

### Annotation Types

Display facts only: their text is never parsed back into accounting numbers.
`WATCH_STREAK` and `RAID` markers are built from the event-local amount; when
the event was admitted to the ledger they are written in the same transaction
as its point event, and a timeline-only frame writes its marker separately
(`Service.RecordPointMarker`).

| Type | Color | Description |
|------|-------|-------------|
| `WATCH_STREAK` | Blue (#45c1ff) | Watch streak earned |
| `RAID` | Tan (#d9a25c) | Raid points earned |
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
transient retry loop. The diagnostic `RewardList` read (see *Watch Streak
milestone observability*) is the second exception to the per-client-ID attempt
count above: it shares this loop but draws every dispatch — across retries and
client-ID fallback alike — from a three-permit per-cycle allowance, so when that
allowance runs out it stops with `ALLOWANCE_EXHAUSTED` before the schedule
completes (its per-retry line and its exhausted-ladder summary are `DEBUG`).

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
