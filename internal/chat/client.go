package chat

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type ChatLogger interface {
	RecordChatMessage(streamer string, msg ChatMessageData) error
}

// MentionHandler is called when the user is mentioned in chat.
type MentionHandler func(streamer, fromUser, message string)

type ChatMessageData struct {
	Username    string
	DisplayName string
	Message     string
	Emotes      string
	Badges      string
	Color       string
}

type IRCClient struct {
	username       string
	token          string
	channel        string
	streamer       *models.Streamer
	logger         ChatLogger
	logChat        bool
	mentionHandler MentionHandler

	conn     net.Conn
	reader   *bufio.Reader
	running  bool
	stopChan chan struct{}

	mu sync.RWMutex
}

func NewIRCClient(username, token string, streamer *models.Streamer, logger ChatLogger, logChat bool, mentionHandler MentionHandler) *IRCClient {
	slog.Debug("Creating IRC client", "channel", streamer.Username, "logChat", logChat, "hasLogger", logger != nil)
	return &IRCClient{
		username:       username,
		token:          token,
		channel:        "#" + strings.ToLower(streamer.Username),
		streamer:       streamer,
		logger:         logger,
		logChat:        logChat,
		mentionHandler: mentionHandler,
		stopChan:       make(chan struct{}),
	}
}

func (c *IRCClient) Connect() error {
	addr := net.JoinHostPort(constants.IRCURL, fmt.Sprintf("%d", constants.IRCPort))
	conn, err := net.DialTimeout("tcp", addr, 30*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to IRC: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.reader = bufio.NewReader(conn)
	c.running = true
	c.mu.Unlock()

	if err := c.authenticate(); err != nil {
		_ = c.conn.Close()
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	if err := c.join(); err != nil {
		_ = c.conn.Close()
		return fmt.Errorf("failed to join channel: %w", err)
	}

	go c.readLoop()

	slog.Info("Joined IRC chat", "channel", c.channel)
	return nil
}

func (c *IRCClient) authenticate() error {
	if c.logChat {
		if err := c.send("CAP REQ :twitch.tv/tags twitch.tv/commands"); err != nil {
			return err
		}
	}
	if err := c.send(fmt.Sprintf("PASS oauth:%s", c.token)); err != nil {
		return err
	}
	return c.send(fmt.Sprintf("NICK %s", c.username))
}

func (c *IRCClient) join() error {
	return c.send(fmt.Sprintf("JOIN %s", c.channel))
}

func (c *IRCClient) send(message string) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	_, err := conn.Write([]byte(message + "\r\n"))
	return err
}

func (c *IRCClient) readLoop() {
	for {
		select {
		case <-c.stopChan:
			return
		default:
		}

		c.mu.RLock()
		reader := c.reader
		running := c.running
		c.mu.RUnlock()

		if !running || reader == nil {
			return
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			c.mu.RLock()
			running := c.running
			c.mu.RUnlock()

			if running {
				slog.Debug("IRC read error", "channel", c.channel, "error", err)
			}
			return
		}

		line = strings.TrimSpace(line)
		c.handleMessage(line)
	}
}

func (c *IRCClient) handleMessage(line string) {
	slog.Debug("IRC message received", "channel", c.channel, "line", line)

	if strings.HasPrefix(line, "PING") {
		pongMsg := strings.Replace(line, "PING", "PONG", 1)
		_ = c.send(pongMsg)
		return
	}

	if strings.Contains(line, "PRIVMSG") {
		c.handlePrivMsg(line)
	}
}

func (c *IRCClient) handlePrivMsg(line string) {
	var tags map[string]string
	remaining := line

	if strings.HasPrefix(line, "@") {
		spaceIdx := strings.Index(line, " ")
		if spaceIdx == -1 {
			return
		}
		tags = parseTags(line[1:spaceIdx])
		remaining = line[spaceIdx+1:]
	}

	parts := strings.SplitN(remaining, " ", 4)
	if len(parts) < 4 {
		return
	}

	prefix := parts[0]
	message := strings.TrimPrefix(parts[3], ":")

	nick := ""
	if strings.HasPrefix(prefix, ":") {
		prefix = prefix[1:]
		if idx := strings.Index(prefix, "!"); idx > 0 {
			nick = prefix[:idx]
		}
	}

	if c.logChat && c.logger != nil {
		displayName := nick
		if dn, ok := tags["display-name"]; ok && dn != "" {
			displayName = dn
		}

		msgData := ChatMessageData{
			Username:    nick,
			DisplayName: displayName,
			Message:     message,
			Emotes:      tags["emotes"],
			Badges:      tags["badges"],
			Color:       tags["color"],
		}

		if err := c.logger.RecordChatMessage(c.streamer.Username, msgData); err != nil {
			slog.Debug("Failed to log chat message", "error", err)
		}
	}

	mention := "@" + strings.ToLower(c.username)
	if strings.Contains(strings.ToLower(message), mention) ||
		strings.Contains(strings.ToLower(message), strings.ToLower(c.username)) {
		slog.Info("Chat mention",
			"channel", c.channel,
			"from", nick,
			"message", message,
		)

		if c.mentionHandler != nil {
			c.mentionHandler(c.streamer.Username, nick, message)
		}
	}
}

func parseTags(tagStr string) map[string]string {
	tags := make(map[string]string)
	for _, tag := range strings.Split(tagStr, ";") {
		parts := strings.SplitN(tag, "=", 2)
		if len(parts) == 2 {
			tags[parts[0]] = parts[1]
		}
	}
	return tags
}

func (c *IRCClient) Stop() {
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()

	close(c.stopChan)

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn != nil {
		_ = c.send("PART " + c.channel)
		_ = conn.Close()
	}

	slog.Info("Left IRC chat", "channel", c.channel)
}

func (c *IRCClient) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}
