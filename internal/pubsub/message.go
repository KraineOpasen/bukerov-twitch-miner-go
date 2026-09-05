package pubsub

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type WSMessage struct {
	Type  string  `json:"type"`
	Nonce string  `json:"nonce,omitempty"`
	Data  *WSData `json:"data,omitempty"`
	Error string  `json:"error,omitempty"`
}

type WSData struct {
	Topics    []string `json:"topics,omitempty"`
	AuthToken string   `json:"auth_token,omitempty"`
	Topic     string   `json:"topic,omitempty"`
	Message   string   `json:"message,omitempty"`
}

type PubSubMessage struct {
	Topic     Topic
	Type      string
	Data      map[string]interface{}
	Message   map[string]interface{}
	Timestamp time.Time
	ChannelID string

	// EventFingerprint is an opaque exact-domain-event identity: SHA-256 of
	// the topic and canonical complete inner JSON message. It is used for
	// transport replay suppression and Stream-owned WATCH_STREAK idempotency,
	// never as broadcast attribution. Derived/fallback Timestamp is excluded.
	//
	// It hashes the RAW inner frame together with an account-scoped topic, so
	// it is transport state only: it must never be persisted or re-hashed into
	// a durable record. The Prediction observation trail derives its own
	// fingerprint from a sanitized projection instead.
	EventFingerprint string

	// Connection provenance: which pooled connection delivered this frame, on
	// which connection generation, and its position in that connection's
	// delivery order. Stamped by the connection immediately before dispatch
	// (see WebSocketClient's TEXT/MESSAGE handling) and read only by
	// observers — nothing in the message-handling path branches on it.
	// ConnectionKnown is false for a message built by a caller that has no
	// connection (a test fixture, a synthesized frame).
	ConnectionIndex      int
	ConnectionGeneration uint64
	ConnectionSequence   uint64
	ConnectionKnown      bool

	// TimestampSource says WHERE Timestamp came from. Timestamp itself is
	// always set — it falls back to the receiver's own clock — so its value
	// alone cannot distinguish a time the producer stated from one this
	// process invented. Nothing in the transport branches on this; it exists
	// so a durable record can say which it has. Empty on a message a caller
	// built directly rather than parsed off the wire.
	TimestampSource string
}

// Where a PubSubMessage.Timestamp came from.
const (
	// TimestampFromProducer: the frame carried its own data.timestamp.
	TimestampFromProducer = "PRODUCER"
	// TimestampFromServer: no producer time, but the envelope carried
	// server_time.
	TimestampFromServer = "SERVER"
	// TimestampFromReceiver: neither was usable, so Timestamp is this
	// process's own clock reading and says nothing about the producer.
	TimestampFromReceiver = "RECEIVER"
)

func ParsePubSubMessage(data *WSData) (*PubSubMessage, error) {
	topic, err := ParseTopic(data.Topic)
	if err != nil {
		return nil, err
	}

	var message map[string]interface{}
	if err := json.Unmarshal([]byte(data.Message), &message); err != nil {
		return nil, err
	}

	msg := &PubSubMessage{
		Topic:            topic,
		Message:          message,
		ChannelID:        topic.ChannelID,
		EventFingerprint: fingerprintPubSubEvent(topic, message),
	}

	if msgType, ok := message["type"].(string); ok {
		msg.Type = msgType
	}

	if msgData, ok := message["data"].(map[string]interface{}); ok {
		msg.Data = msgData
	}

	msg.Timestamp, msg.TimestampSource = extractTimestamp(message, msg.Data)

	if msg.Data != nil {
		msg.ChannelID = extractChannelID(msg.Data, topic.ChannelID)
	}

	return msg, nil
}

func fingerprintPubSubEvent(topic Topic, message map[string]interface{}) string {
	canonical, err := json.Marshal(message)
	if err != nil {
		return ""
	}
	h := sha256.New()
	_, _ = h.Write([]byte("twitch-pubsub-v1\x00"))
	_, _ = h.Write([]byte(topic.String()))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// extractTimestamp returns the frame's best available time AND where that time
// came from. The value is unchanged from what this function has always
// returned; only the provenance is new. The fallback to the local clock is what
// makes the provenance necessary: without it every frame looks producer-timed,
// including the ones this process timed itself.
func extractTimestamp(message, data map[string]interface{}) (time.Time, string) {
	if data != nil {
		if ts, ok := data["timestamp"].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				return t, TimestampFromProducer
			}
		}
	}

	if ts, ok := message["server_time"].(float64); ok {
		return time.Unix(int64(ts), 0), TimestampFromServer
	}

	return time.Now(), TimestampFromReceiver
}

func extractChannelID(data map[string]interface{}, defaultID string) string {
	if prediction, ok := data["prediction"].(map[string]interface{}); ok {
		if id, ok := prediction["channel_id"].(string); ok {
			return id
		}
	}
	if claim, ok := data["claim"].(map[string]interface{}); ok {
		if id, ok := claim["channel_id"].(string); ok {
			return id
		}
	}
	if id, ok := data["channel_id"].(string); ok {
		return id
	}
	if balance, ok := data["balance"].(map[string]interface{}); ok {
		if id, ok := balance["channel_id"].(string); ok {
			return id
		}
	}
	return defaultID
}
