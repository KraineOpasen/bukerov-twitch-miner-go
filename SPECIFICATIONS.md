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
│   └── miner.go                # Coordinates all components, context-based lifecycle
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
│   └── watcher.go              # Simulates viewing, reports to Twitch
│
├── drops/                      # Game drops tracking
│   └── drops.go                # Campaign sync, drop claiming
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
| `web` | HTTP server for dashboard UI. Optional basic auth via environment variables. |
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
- Tokens persisted locally between sessions
- Contains: `auth-token`, `user_id`, session metadata
- Format: Application-specific (cookies, JSON, etc.)

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
   Body: base64_encoded(json_payload)
```

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

### Priority System

Maximum 2 streams watched simultaneously (`constants.MaxSimultaneousStreams`).

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
- **Priority as a boost, not exclusivity:** on top of the weighted base pair, any online streamer with an active drop (`DROPS`) or an in-progress watch streak (`STREAK`) can take over one seat in the pair for the current tick only, without affecting the weighting above - increasing how often it's picked, never granting a permanent exclusive slot. The seat sacrificed is whichever base-pair member was watched most recently.
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
└── drops: Drop[]
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
   └── Filter by date range

2. Sync Inventory
   ├── GET Inventory
   ├── Match drops to campaigns
   └── Update progress

3. Check Claimable
   ├── dropInstanceId != null
   ├── isClaimed == false
   └── currentMinutesWatched >= requiredMinutesWatched

4. Claim Drop
   ├── POST DropsPage_ClaimDropRewards
   └── Mark as claimed
```

### Drops Eligibility

A streamer is eligible for drops when:
- `claimDrops` setting is enabled
- Streamer is online
- Stream has active campaign IDs
- Campaign game matches stream game

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

-- Indexes for performance
CREATE INDEX idx_points_streamer_time ON points(streamer_id, timestamp);
CREATE INDEX idx_annotations_streamer_time ON annotations(streamer_id, timestamp);
CREATE INDEX idx_chat_streamer_time ON chat_messages(streamer_id, timestamp);
```

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

#### Query Parameters for `/json/{streamer}`
- `startDate`: Filter start (YYYY-MM-DD)
- `endDate`: Filter end (YYYY-MM-DD)

#### Query Parameters for `/api/chat/{streamer}`
- `limit`: Max messages to return (default: 50, max: 200)
- `offset`: Pagination offset
- `q`: Search query (searches message, username, display name)

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
| `campaignSyncInterval` | int | 60 | Minutes between drop campaign syncs (5-120) |
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
| `campaignSyncInterval` | 60 | 5 | 120 | Minutes between drop campaign syncs |
| `minuteWatchedInterval` | 60 | 30 | 120 | Base seconds for minute-watched cycle (divided by # streamers, ±20% jitter) |
| `requestDelay` | 0.5 | 0.1 | 2.0 | Seconds between consecutive API calls |
| `reconnectDelay` | 60 | 30 | 300 | Seconds to wait before reconnecting |
| `streamCheckInterval` | 600 | 60 | 900 | Seconds between stream status checks |
| `rotationIntervalMinMinutes` | 30 | 5 | 180 | Minutes the watch pair dwells before rotating (minimum of the random range) |
| `rotationIntervalMaxMinutes` | 80 | 5 | 240 | Minutes the watch pair dwells before rotating (maximum of the random range; clamped up to Min if set lower) |

---

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
