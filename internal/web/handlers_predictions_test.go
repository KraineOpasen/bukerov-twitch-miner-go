package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubPredictionControl struct {
	betTitle string
	betErr   error
	betCalls int
	lastBet  struct {
		eventID   string
		outcomeID string
		amount    int
	}

	skipErr   error
	skipCalls int
	lastSkip  struct {
		eventID string
		skip    bool
	}
}

func (s *stubPredictionControl) PlaceManualBet(eventID, outcomeID string, amount int) (string, error) {
	s.betCalls++
	s.lastBet.eventID = eventID
	s.lastBet.outcomeID = outcomeID
	s.lastBet.amount = amount
	return s.betTitle, s.betErr
}

func (s *stubPredictionControl) SetAutoBetSkip(eventID string, skip bool) error {
	s.skipCalls++
	s.lastSkip.eventID = eventID
	s.lastSkip.skip = skip
	return s.skipErr
}

func newPredictionServer(p PredictionControlProvider) *Server {
	s := &Server{status: NewStatusBroadcaster()}
	s.predictionControl = p
	return s
}

func postJSON(path, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
}

func TestPredictionBetSuccess(t *testing.T) {
	p := &stubPredictionControl{betTitle: "Yes"}
	s := newPredictionServer(p)

	rr := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr, postJSON("/api/prediction/bet", `{"eventId":"e1","outcomeId":"o1","amount":500}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("success = %v, want true", resp["success"])
	}
	if resp["outcome"] != "Yes" {
		t.Errorf("outcome = %v, want Yes", resp["outcome"])
	}
	if p.lastBet.eventID != "e1" || p.lastBet.outcomeID != "o1" || p.lastBet.amount != 500 {
		t.Errorf("provider got %+v", p.lastBet)
	}
}

func TestPredictionBetDomainFailureIsHTTP200(t *testing.T) {
	p := &stubPredictionControl{betErr: errors.New("not enough channel points for that bet")}
	s := newPredictionServer(p)

	rr := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr, postJSON("/api/prediction/bet", `{"eventId":"e1","outcomeId":"o1","amount":500}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("domain failure should be 200, got %d", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != false {
		t.Errorf("success = %v, want false", resp["success"])
	}
	if resp["message"] != "not enough channel points for that bet" {
		t.Errorf("message = %v, want the friendly error passed through", resp["message"])
	}
}

func TestPredictionBetRoundClosedDuringOp(t *testing.T) {
	p := &stubPredictionControl{betErr: errors.New("this prediction has already closed")}
	s := newPredictionServer(p)

	rr := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr, postJSON("/api/prediction/bet", `{"eventId":"e1","outcomeId":"o1","amount":500}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != false || resp["message"] != "this prediction has already closed" {
		t.Errorf("unexpected response: %v", resp)
	}
}

func TestPredictionBetRepeatedRequest(t *testing.T) {
	p := &stubPredictionControl{betTitle: "Yes"}
	s := newPredictionServer(p)

	// First call succeeds.
	rr1 := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr1, postJSON("/api/prediction/bet", `{"eventId":"e1","outcomeId":"o1","amount":500}`))
	// Second identical call: the provider now reports the round already has a bet.
	p.betErr = errors.New("a bet has already been placed on this prediction")
	rr2 := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr2, postJSON("/api/prediction/bet", `{"eventId":"e1","outcomeId":"o1","amount":500}`))

	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d", rr2.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr2.Body.Bytes(), &resp)
	if resp["success"] != false || !strings.Contains(resp["message"].(string), "already been placed") {
		t.Errorf("repeated request should be reported as already placed, got %v", resp)
	}
	if p.betCalls != 2 {
		t.Errorf("provider betCalls = %d, want 2", p.betCalls)
	}
}

func TestPredictionBetValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing eventId", `{"outcomeId":"o1","amount":500}`},
		{"missing outcomeId", `{"eventId":"e1","amount":500}`},
		{"blank eventId", `{"eventId":"  ","outcomeId":"o1","amount":500}`},
		{"malformed json", `{"eventId":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &stubPredictionControl{}
			s := newPredictionServer(p)
			rr := httptest.NewRecorder()
			s.handleAPIPredictionBet(rr, postJSON("/api/prediction/bet", tc.body))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rr.Code)
			}
			if p.betCalls != 0 {
				t.Errorf("provider must not be called for a structurally invalid request")
			}
		})
	}
}

func TestPredictionBetWrongMethod(t *testing.T) {
	s := newPredictionServer(&stubPredictionControl{})
	rr := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr, httptest.NewRequest(http.MethodGet, "/api/prediction/bet", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestPredictionBetNoProvider(t *testing.T) {
	s := &Server{status: NewStatusBroadcaster()}
	rr := httptest.NewRecorder()
	s.handleAPIPredictionBet(rr, postJSON("/api/prediction/bet", `{"eventId":"e1","outcomeId":"o1","amount":500}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestPredictionSkipSuccess(t *testing.T) {
	p := &stubPredictionControl{}
	s := newPredictionServer(p)

	rr := httptest.NewRecorder()
	s.handleAPIPredictionSkip(rr, postJSON("/api/prediction/skip", `{"eventId":"e1","skip":true}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != true || resp["skip"] != true {
		t.Errorf("unexpected response: %v", resp)
	}
	if p.lastSkip.eventID != "e1" || !p.lastSkip.skip {
		t.Errorf("provider got %+v", p.lastSkip)
	}
}

func TestPredictionSkipUndo(t *testing.T) {
	p := &stubPredictionControl{}
	s := newPredictionServer(p)

	rr := httptest.NewRecorder()
	s.handleAPIPredictionSkip(rr, postJSON("/api/prediction/skip", `{"eventId":"e1","skip":false}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if p.lastSkip.skip {
		t.Error("skip should be false for an undo")
	}
}

func TestPredictionSkipFailure(t *testing.T) {
	p := &stubPredictionControl{skipErr: errors.New("this prediction has already closed")}
	s := newPredictionServer(p)

	rr := httptest.NewRecorder()
	s.handleAPIPredictionSkip(rr, postJSON("/api/prediction/skip", `{"eventId":"e1","skip":true}`))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["success"] != false || resp["message"] != "this prediction has already closed" {
		t.Errorf("unexpected response: %v", resp)
	}
}

func TestPredictionSkipValidation(t *testing.T) {
	p := &stubPredictionControl{}
	s := newPredictionServer(p)
	rr := httptest.NewRecorder()
	s.handleAPIPredictionSkip(rr, postJSON("/api/prediction/skip", `{"skip":true}`))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	if p.skipCalls != 0 {
		t.Error("provider must not be called without eventId")
	}
}

func TestPredictionSkipWrongMethod(t *testing.T) {
	s := newPredictionServer(&stubPredictionControl{})
	rr := httptest.NewRecorder()
	s.handleAPIPredictionSkip(rr, httptest.NewRequest(http.MethodGet, "/api/prediction/skip", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}
