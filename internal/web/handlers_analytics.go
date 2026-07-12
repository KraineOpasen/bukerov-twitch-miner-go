package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
)

func (s *Server) handleStreamers(w http.ResponseWriter, r *http.Request) {
	repo := s.analytics.Repository()
	streamers, err := repo.ListStreamers()
	if err != nil {
		writeInternalError(w, "Failed to list streamers")
		return
	}

	writeJSONOK(w, streamers)
}

func (s *Server) handleJSON(w http.ResponseWriter, r *http.Request) {
	streamer := strings.TrimPrefix(r.URL.Path, "/json/")
	streamer = strings.TrimSuffix(streamer, ".json")

	if streamer == "" {
		writeBadRequest(w, "Streamer not specified")
		return
	}

	startDate := r.URL.Query().Get("startDate")
	endDate := r.URL.Query().Get("endDate")

	var startTime, endTime time.Time
	if startDate != "" {
		if t, err := time.Parse("2006-01-02", startDate); err == nil {
			startTime = t
		}
	}
	if endDate != "" {
		if t, err := time.Parse("2006-01-02", endDate); err == nil {
			endTime = t.Add(24*time.Hour - time.Second)
		}
	}

	repo := s.analytics.Repository()
	var data *analytics.StreamerData
	var err error
	if !startTime.IsZero() || !endTime.IsZero() {
		data, err = repo.GetStreamerDataFiltered(streamer, startTime, endTime)
	} else {
		data, err = repo.GetStreamerData(streamer)
	}

	if err != nil {
		writeInternalError(w, "Failed to get data")
		return
	}

	writeJSONOK(w, data)
}

func (s *Server) handleJSONAll(w http.ResponseWriter, r *http.Request) {
	repo := s.analytics.Repository()
	streamers, err := repo.ListStreamers()
	if err != nil {
		writeInternalError(w, "Failed to list streamers")
		return
	}

	type namedData struct {
		Name string                 `json:"name"`
		Data analytics.StreamerData `json:"data"`
	}

	var result []namedData
	for _, info := range streamers {
		data, err := repo.GetStreamerData(info.Name)
		if err != nil {
			continue
		}
		result = append(result, namedData{Name: info.Name, Data: *data})
	}

	writeJSONOK(w, result)
}

func (s *Server) handleAPIChatMessages(w http.ResponseWriter, r *http.Request) {
	streamer := strings.TrimPrefix(r.URL.Path, "/api/chat/")
	if streamer == "" {
		writeBadRequest(w, "Streamer not specified")
		return
	}

	limit := 50
	offset := 0
	query := r.URL.Query().Get("q")

	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
			if limit > 200 {
				limit = 200
			}
		}
	}

	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	repo := s.analytics.Repository()
	var data *analytics.ChatLogData
	var err error

	if query != "" {
		data, err = repo.SearchChatMessages(streamer, query, limit, offset)
	} else {
		data, err = repo.GetChatMessages(streamer, limit, offset)
	}

	if err != nil {
		writeInternalError(w, "Failed to get chat messages")
		return
	}

	writeJSONOK(w, data)
}
