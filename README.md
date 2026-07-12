<p align="center">
  <img src="assets/banner.jpg" alt="Twitch Points Miner" width="640">
</p>

# Twitch Channel Points Miner

[![CI](https://github.com/PatrickWalther/twitch-miner-go/actions/workflows/ci.yml/badge.svg)](https://github.com/PatrickWalther/twitch-miner-go/actions/workflows/ci.yml)
[![Release](https://github.com/PatrickWalther/twitch-miner-go/actions/workflows/release.yml/badge.svg)](https://github.com/PatrickWalther/twitch-miner-go/releases)
[![Release](https://img.shields.io/github/v/release/KraineOpasen/bukerov-twitch-miner-go?label=release)](https://github.com/KraineOpasen/bukerov-twitch-miner-go/releases)

A fork of [PatrickWalther/twitch-miner-go](https://github.com/PatrickWalther/twitch-miner-go).

This tool passively earns Twitch channel points by simulating viewer presence across multiple streams.

## Features

- **Passive Point Farming**: Earn channel points (+10-12 every 5 minutes) by simulating watch time
- **Automatic Bonus Claiming**: Auto-claim +50 point bonuses when available
- **Watch Streak Detection**: Catch +450 point watch streaks across streamers
- **Raid Following**: Automatically join raids for +250 points
- **Prediction Betting**: Intelligent automated betting on channel predictions with multiple strategies
- **Game Drops**: Track and claim game drops from inventory
- **Moments Claiming**: Automatically claim Twitch Moments when available
- **Community Goals**: Contribute channel points to streamer community goals
- **Multi-Streamer Support**: Monitor multiple streamers with priority-based scheduling
- **Real-time Analytics**: Web-based dashboard for tracking point earnings
- **Discord Notifications**: Get notified for mentions, point goals, and stream status changes

## Quick Start

Choose one of the four installation methods below.

| Method | Best For |
|--------|----------|
| [Docker](#option-1-docker-recommended) | Most users, easiest setup |
| [Binary Download](#option-2-binary-download) | Running natively without Docker |
| [Unraid](#option-3-unraid) | Unraid server users |
| [Build from Source](#option-4-build-from-source) | Developers, contributors |

---

## Option 1: Docker (Recommended)

New to Docker? See [Docker's getting started guide](https://docs.docker.com/get-started/) or install [Docker Desktop](https://docs.docker.com/desktop/) for Windows/macOS.

### Step 1: Create directories and config

```bash
# Create directories for persistent data
mkdir -p ~/twitch-miner/{config,cookies,logs,database}

# Create a minimal config file
cat > ~/twitch-miner/config/config.json << 'EOF'
{
  "username": "your_twitch_username",
  "enableAnalytics": true,
  "streamers": [
    { "username": "streamer1" },
    { "username": "streamer2" }
  ]
}
EOF
```

Edit `~/twitch-miner/config/config.json` with your Twitch username and the streamers you want to watch.

### Step 2: Run the container

```bash
docker run -d \
  --name twitch-miner \
  -v ~/twitch-miner/config:/config \
  -v ~/twitch-miner/cookies:/cookies \
  -v ~/twitch-miner/logs:/logs \
  -v ~/twitch-miner/database:/database \
  -p 5000:5000 \
  thegame402/twitch-miner-go:latest
```

### Step 3: Authenticate with Twitch

Open http://localhost:5000 in your browser. The dashboard will show the authentication prompt with a code and link.

1. Click the link or go to https://www.twitch.tv/activate
2. Enter the code displayed
3. The miner will automatically continue once authenticated

**Running headless?** View the authentication code in the logs instead:
```bash
docker logs -f twitch-miner
```

### Step 4: Access the dashboard

Once authenticated, the dashboard shows all your streamers, points, and earnings.

### Optional: Protect the dashboard with authentication

```bash
docker run -d \
  --name twitch-miner \
  -e DASHBOARD_USERNAME=admin \
  -e DASHBOARD_PASSWORD=your-secure-password \
  -v ~/twitch-miner/config:/config \
  -v ~/twitch-miner/cookies:/cookies \
  -v ~/twitch-miner/logs:/logs \
  -v ~/twitch-miner/database:/database \
  -p 5000:5000 \
  thegame402/twitch-miner-go:latest
```

### Alternative: Use GitHub Container Registry

```bash
docker pull ghcr.io/patrickwalther/twitch-miner-go:latest
```

---

## Option 2: Binary Download

### Step 1: Download the binary

Download the latest release for your platform from the [Releases page](https://github.com/PatrickWalther/twitch-miner-go/releases).

| Platform | File |
|----------|------|
| Windows | `twitch-miner-go-windows-amd64.exe` |
| Linux (x64) | `twitch-miner-go-linux-amd64` |
| Linux (ARM64) | `twitch-miner-go-linux-arm64` |
| macOS (Intel) | `twitch-miner-go-darwin-amd64` |
| macOS (Apple Silicon) | `twitch-miner-go-darwin-arm64` |

### Step 2: Generate a config file

```bash
# Linux/macOS
./twitch-miner-go -generate-config

# Windows
.\twitch-miner-go.exe -generate-config
```

This creates `config.sample.json`. Rename it to `config.json`:

```bash
mv config.sample.json config.json
```

### Step 3: Edit the config

Open `config.json` and set your Twitch username and streamers:

```json
{
  "username": "your_twitch_username",
  "enableAnalytics": true,
  "streamers": [
    { "username": "streamer1" },
    { "username": "streamer2" }
  ]
}
```

See [Configuration Reference](#configuration-reference) for all options.

### Step 4: Run the miner

```bash
# Linux/macOS
./twitch-miner-go

# Windows
.\twitch-miner-go.exe
```

### Step 5: Authenticate with Twitch

Open http://localhost:5000 in your browser. The dashboard will show the authentication prompt with a code and link.

1. Click the link or go to https://www.twitch.tv/activate
2. Enter the code displayed
3. The miner will automatically continue once authenticated

### Step 6: Use the dashboard

Once authenticated, the dashboard shows all your streamers, points, and earnings.

### Command-line options

| Flag | Description |
|------|-------------|
| `-config path/to/config.json` | Use a custom config file location |
| `-debug` | Enable debug logging |
| `-generate-config` | Generate a sample configuration file |

---

## Option 3: Unraid

### Via Community Applications (Recommended)

1. Install the **Community Applications** plugin from the Apps tab
2. Search for "twitch-miner-go" in Community Applications
3. If no results appear, click the **DockerHub** button next to the search field
4. Click Install and configure paths
5. After installation, edit the config file at your configured path

### Manual Installation

1. Go to Docker tab → Add Container
2. Set repository to: `thegame402/twitch-miner-go:latest`
3. Add the following path mappings:

| Container Path | Host Path | Description |
|----------------|-----------|-------------|
| `/config` | `/mnt/user/appdata/twitch-miner/config` | Config file |
| `/cookies` | `/mnt/user/appdata/twitch-miner/cookies` | Auth tokens |
| `/logs` | `/mnt/user/appdata/twitch-miner/logs` | Log files |
| `/database` | `/mnt/user/appdata/twitch-miner/database` | SQLite database |

4. Add port mapping: `5000` → `5000`
5. Create a config file at `/mnt/user/appdata/twitch-miner/config/config.json`

---

## Option 4: Build from Source

### Prerequisites

- Go 1.24 or later
- Git
- Make (optional, for using Makefile targets)

### Step 1: Clone and build

```bash
git clone https://github.com/PatrickWalther/twitch-miner-go.git
cd twitch-miner-go

# Build with version info (includes Tailwind CSS build)
make build

# Or build with UPX compression for smallest size
make build-compressed

# Or build manually (requires Tailwind CSS to be pre-built)
go build -ldflags "-s -w -X github.com/PatrickWalther/twitch-miner-go/internal/version.Version=$(git describe --tags)" -o twitch-miner-go ./cmd/miner
```

### Step 2: Generate config and run

```bash
./twitch-miner-go -generate-config
mv config.sample.json config.json
# Edit config.json with your settings
./twitch-miner-go
```

### Cross-compilation

```bash
# Build for all platforms
make build-all

# Or build for specific platforms
make build-linux        # Linux x64
make build-linux-arm64  # Linux ARM64
make build-windows      # Windows x64
make build-darwin       # macOS Intel
make build-darwin-arm64 # macOS Apple Silicon
```

### Build Docker image locally

```bash
make docker
```

---

## Web Dashboard

When `enableAnalytics` is true, the miner provides a web dashboard at http://localhost:5000 with:

- **Dashboard**: Overview of all streamers with current points and today's earnings
- **Streamer Pages**: Historical point data with interactive charts
- **Settings**: Runtime configuration that can be changed without restart
- **Notifications**: Discord notification management (when Discord is enabled)
- **Chat Logs**: Searchable chat history per streamer (when enabled)

### Managing Settings via Web Dashboard

Instead of editing `config.json` manually, you can change most settings through the **Settings** page in the dashboard. Changes take effect immediately without restarting the miner.

---

## Configuration Reference

Generate a sample config with all options:

```bash
./twitch-miner-go -generate-config
```

### Minimal Config

```json
{
  "username": "your_twitch_username",
  "enableAnalytics": true,
  "streamers": [
    { "username": "streamer1" },
    { "username": "streamer2" }
  ]
}
```

### Full Config Structure

<details>
<summary>Click to expand full configuration example</summary>

```json
{
  "username": "your_twitch_username",
  "claimDropsOnStartup": false,
  "enableAnalytics": true,
  "priority": ["STREAK", "DROPS", "ORDER"],
  "streamerSettings": {
    "makePredictions": true,
    "followRaid": true,
    "claimDrops": true,
    "claimMoments": true,
    "watchStreak": true,
    "communityGoals": false,
    "chat": "ONLINE",
    "bet": {
      "strategy": "SMART",
      "percentage": 5,
      "percentageGap": 20,
      "maxPoints": 50000,
      "minimumPoints": 0,
      "stealthMode": false,
      "delay": 6,
      "delayMode": "FROM_END"
    }
  },
  "streamers": [
    { "username": "streamer1" },
    { 
      "username": "streamer2",
      "settings": {
        "makePredictions": false
      }
    }
  ],
  "analytics": {
    "host": "0.0.0.0",
    "port": 5000,
    "refresh": 5,
    "daysAgo": 7,
    "enableChatLogs": false
  },
  "discord": {
    "enabled": false,
    "botToken": "",
    "guildId": ""
  },
  "rateLimits": {
    "websocketPingInterval": 27,
    "campaignSyncInterval": 60,
    "minuteWatchedInterval": 60,
    "requestDelay": 0.5,
    "reconnectDelay": 60,
    "streamCheckInterval": 600
  },
  "logger": {
    "save": true,
    "less": false,
    "consoleLevel": "INFO",
    "fileLevel": "DEBUG",
    "colored": false,
    "autoClear": true
  }
}
```

</details>

### Priority System

The miner watches up to 2 streams simultaneously, selected by priority order:

| Priority | Behavior |
|----------|----------|
| `STREAK` | Prioritize streamers with pending watch streak |
| `DROPS` | Prioritize streamers with active drop campaigns |
| `SUBSCRIBED` | Prioritize subscribed channels |
| `ORDER` | Follow order in streamers list |
| `POINTS_ASCENDING` | Lowest points first |
| `POINTS_DESCENDING` | Highest points first |

### Streamer Settings

Applied globally via `streamerSettings`, can be overridden per-streamer:

| Setting | Default | Description |
|---------|---------|-------------|
| `makePredictions` | true | Enable betting on predictions |
| `followRaid` | true | Automatically join raids |
| `claimDrops` | true | Claim game drops |
| `claimMoments` | true | Claim Twitch Moments |
| `watchStreak` | true | Prioritize watch streaks |
| `communityGoals` | false | Contribute to community goals |
| `chat` | ONLINE | When to join IRC chat |
| `chatLogs` | null | Override global chat logging |

### Chat Presence Modes

| Mode | Behavior |
|------|----------|
| `ALWAYS` | Always connected to IRC |
| `NEVER` | Never connect to IRC |
| `ONLINE` | Connect when streamer is online |
| `OFFLINE` | Connect when streamer is offline |

### Betting Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `strategy` | SMART | Betting strategy (see below) |
| `percentage` | 5 | Percentage of balance to bet |
| `percentageGap` | 20 | Gap threshold for SMART strategy |
| `maxPoints` | 50000 | Maximum points per bet |
| `minimumPoints` | 0 | Minimum points required to bet |
| `stealthMode` | false | Stay below highest bet |
| `delay` | 6 | Delay before placing bet |
| `delayMode` | FROM_END | How delay is calculated |

#### Betting Strategies

| Strategy | Logic |
|----------|-------|
| `SMART` | If user gap > percentageGap: follow majority; else: choose highest odds |
| `MOST_VOTED` | Choose option with most users |
| `HIGH_ODDS` | Choose option with highest odds |
| `PERCENTAGE` | Choose option with highest win percentage |
| `SMART_MONEY` | Choose option with highest top bet |
| `NUMBER_1` - `NUMBER_8` | Always choose specific outcome position |

#### Delay Modes

| Mode | Behavior |
|------|----------|
| `FROM_START` | Wait X seconds after prediction opens |
| `FROM_END` | Place bet X seconds before prediction closes |
| `PERCENTAGE` | Wait X% of the prediction window |

### Analytics Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `host` | 0.0.0.0 | Server bind address |
| `port` | 5000 | Server port |
| `refresh` | 5 | Dashboard auto-refresh interval (minutes) |
| `daysAgo` | 7 | Default chart date range |
| `enableChatLogs` | false | Enable chat message logging |

### Rate Limits

Defaults are tuned to avoid Twitch rate limiting:

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| `websocketPingInterval` | 27 | 20-60 | Seconds between WebSocket pings |
| `campaignSyncInterval` | 60 | 5-120 | Minutes between drop campaign syncs |
| `minuteWatchedInterval` | 60 | 30-120 | Seconds for minute-watched cycle |
| `requestDelay` | 0.5 | 0.1-2.0 | Seconds between API calls |
| `reconnectDelay` | 60 | 30-300 | Seconds before reconnecting |
| `streamCheckInterval` | 600 | 60-900 | Seconds between status checks |

---

## Discord Notifications

The miner supports Discord notifications for chat mentions, point goals, and stream status changes.

### Step 1: Create a Discord Bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications)
2. Click **New Application** and give it a name
3. Go to **Bot** section
4. Click **Reset Token** and copy the token (keep it secret!)
5. Enable **Message Content Intent** under Privileged Gateway Intents

### Step 2: Invite the Bot

1. Go to **OAuth2 → URL Generator**
2. Select the **bot** scope
3. Select permissions: Send Messages, Embed Links, Read Message History
4. Copy the URL and open it in your browser
5. Select your server and authorize

### Step 3: Get Your Guild ID

1. Enable Developer Mode in Discord (User Settings → Advanced → Developer Mode)
2. Right-click your server name → **Copy Server ID**

See [Discord's official guide](https://support.discord.com/hc/en-us/articles/206346498-Where-can-I-find-my-User-Server-Message-ID) for detailed instructions.

### Step 4: Add to Config

```json
{
  "discord": {
    "enabled": true,
    "botToken": "YOUR_BOT_TOKEN",
    "guildId": "YOUR_SERVER_ID"
  }
}
```

### Step 5: Configure Notifications

After restarting with Discord enabled, a **Notifications** page appears in the dashboard where you can:

- Select Discord channels for each notification type
- Enable/disable mention notifications (globally or per-streamer)
- Create point goal rules (one-time or recurring)
- Enable/disable online/offline notifications

---

## Data Storage

The miner creates the following directories:

| Directory | Contents |
|-----------|----------|
| `config/` | Configuration file |
| `cookies/` | Authentication tokens |
| `logs/` | Log files (7-day rotation) |
| `database/` | SQLite database (analytics, notifications) |

---

## License

GNU General Public License v3.0 - See LICENSE file for details.

## Disclaimer

This tool is for educational purposes. Use responsibly and in accordance with Twitch's Terms of Service.
