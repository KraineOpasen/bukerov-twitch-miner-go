package analytics

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/chat"

type ChatLoggerAdapter struct {
	service *Service
}

func NewChatLoggerAdapter(service *Service) *ChatLoggerAdapter {
	return &ChatLoggerAdapter{service: service}
}

func (a *ChatLoggerAdapter) RecordChatMessage(streamer string, msg chat.ChatMessageData) error {
	return a.service.RecordChatMessage(streamer, msg.Username, msg.DisplayName, msg.Message, msg.Emotes, msg.Badges, msg.Color)
}
