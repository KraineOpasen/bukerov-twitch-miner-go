package models

import (
	"math"
	"math/rand"
)

type Strategy string

const (
	StrategyMostVoted  Strategy = "MOST_VOTED"
	StrategyHighOdds   Strategy = "HIGH_ODDS"
	StrategyPercentage Strategy = "PERCENTAGE"
	StrategySmartMoney Strategy = "SMART_MONEY"
	StrategySmart      Strategy = "SMART"
	StrategyNumber1    Strategy = "NUMBER_1"
	StrategyNumber2    Strategy = "NUMBER_2"
	StrategyNumber3    Strategy = "NUMBER_3"
	StrategyNumber4    Strategy = "NUMBER_4"
	StrategyNumber5    Strategy = "NUMBER_5"
	StrategyNumber6    Strategy = "NUMBER_6"
	StrategyNumber7    Strategy = "NUMBER_7"
	StrategyNumber8    Strategy = "NUMBER_8"
)

type Condition string

const (
	ConditionGT  Condition = "GT"
	ConditionLT  Condition = "LT"
	ConditionGTE Condition = "GTE"
	ConditionLTE Condition = "LTE"
)

type DelayMode string

const (
	DelayModeFromStart  DelayMode = "FROM_START"
	DelayModeFromEnd    DelayMode = "FROM_END"
	DelayModePercentage DelayMode = "PERCENTAGE"
)

type OutcomeKey string

const (
	OutcomePercentageUsers OutcomeKey = "percentage_users"
	OutcomeOddsPercentage  OutcomeKey = "odds_percentage"
	OutcomeOdds            OutcomeKey = "odds"
	OutcomeTopPoints       OutcomeKey = "top_points"
	OutcomeTotalUsers      OutcomeKey = "total_users"
	OutcomeTotalPoints     OutcomeKey = "total_points"
	OutcomeDecisionUsers   OutcomeKey = "decision_users"
	OutcomeDecisionPoints  OutcomeKey = "decision_points"
)

type FilterCondition struct {
	By    OutcomeKey `json:"by"`
	Where Condition  `json:"where"`
	Value float64    `json:"value"`
}

type BetSettings struct {
	Strategy        Strategy         `json:"strategy"`
	Percentage      int              `json:"percentage"`
	PercentageGap   int              `json:"percentageGap"`
	MaxPoints       int              `json:"maxPoints"`
	MinimumPoints   int              `json:"minimumPoints"`
	StealthMode     bool             `json:"stealthMode"`
	FilterCondition *FilterCondition `json:"filterCondition,omitempty"`
	Delay           float64          `json:"delay"`
	DelayMode       DelayMode        `json:"delayMode"`

	// Prediction risk gates (auto-bet only, stateless). They clamp the
	// post-Stealth stake Calculate produces; none of them affects manual bets.
	// MaxStakePercent caps the stake to this percentage of the current balance
	// (0 = off). ReservePoints keeps at least this many points unbet — a stake
	// may not push the balance below it (0 = off). HealthGateEnabled blocks
	// auto-bets while the transport is degraded/failed (default on); an unknown
	// health state fails open (bet allowed). The absolute cap is MaxPoints
	// itself — there is no separate absolute-stake field.
	MaxStakePercent   int  `json:"maxStakePercent"`
	ReservePoints     int  `json:"reservePoints"`
	HealthGateEnabled bool `json:"healthGateEnabled"`
}

func DefaultBetSettings() BetSettings {
	return BetSettings{
		Strategy:          StrategySmart,
		Percentage:        5,
		PercentageGap:     20,
		MaxPoints:         50000,
		MinimumPoints:     0,
		StealthMode:       false,
		Delay:             6,
		DelayMode:         DelayModeFromEnd,
		MaxStakePercent:   0,
		ReservePoints:     0,
		HealthGateEnabled: true,
	}
}

// GateReason identifies which prediction risk gate bound (clamped) or blocked an
// auto-bet stake. Empty means no gate changed the proposed amount.
type GateReason string

const (
	GateNone     GateReason = ""
	GateHealth   GateReason = "health"            // transport degraded/failed (applied by the caller)
	GateReserve  GateReason = "reserve_points"    // reservePoints floor
	GatePercent  GateReason = "max_stake_percent" // maxStakePercent cap
	GateAbsolute GateReason = "max_points"        // MaxPoints absolute cap (single source)
)

// ClampStake applies the stateless size gates to a post-Stealth proposed stake:
// the optional reserve floor, the optional max-stake-percent cap, and the
// absolute MaxPoints cap (the single source of the absolute limit — there is no
// separate maxStakePoints field). It returns the allowed stake, the binding
// reason, and that reason's limit value. The tightest cap wins; equal caps break
// toward the higher-priority reason, giving the fixed priority
// reserve > percent > absolute. The health gate is NOT applied here — it is
// stateful and handled by the auto-bet caller first, at a strictly higher
// priority. A non-positive setting disables that gate.
func (s BetSettings) ClampStake(proposed, balance int) (allowed int, reason GateReason, limit int) {
	allowed = proposed
	reason = GateNone

	consider := func(on bool, cap int, r GateReason) {
		if on && cap < allowed {
			allowed = cap
			reason = r
			limit = cap
		}
	}

	// Reserve floor: the stake may not push the balance below ReservePoints.
	reserveCap := balance - s.ReservePoints
	if reserveCap < 0 {
		reserveCap = 0
	}
	consider(s.ReservePoints > 0, reserveCap, GateReserve)

	// Percentage-of-balance cap.
	pct := s.MaxStakePercent
	if pct > 100 {
		pct = 100
	}
	consider(s.MaxStakePercent > 0, balance*pct/100, GatePercent)

	// Absolute cap (single source: MaxPoints). Calculate already applies it, so
	// this is normally a no-op; kept so the gate is self-contained.
	consider(s.MaxPoints > 0, s.MaxPoints, GateAbsolute)

	if allowed < 0 {
		allowed = 0
	}
	return allowed, reason, limit
}

type Outcome struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Color           string  `json:"color"`
	TotalUsers      int     `json:"total_users"`
	TotalPoints     int     `json:"total_points"`
	TopPoints       int     `json:"top_points"`
	PercentageUsers float64 `json:"percentage_users"`
	Odds            float64 `json:"odds"`
	OddsPercentage  float64 `json:"odds_percentage"`
}

func NewOutcomeFromGQL(data map[string]interface{}) *Outcome {
	o := &Outcome{}

	if id, ok := data["id"].(string); ok {
		o.ID = id
	}
	if title, ok := data["title"].(string); ok {
		o.Title = title
	}
	if color, ok := data["color"].(string); ok {
		o.Color = color
	}
	if users, ok := data["total_users"].(float64); ok {
		o.TotalUsers = int(users)
	}
	if points, ok := data["total_points"].(float64); ok {
		o.TotalPoints = int(points)
	}

	return o
}

type Decision struct {
	Choice int
	Amount int
	ID     string
}

type Bet struct {
	Outcomes    []*Outcome
	Decision    Decision
	TotalUsers  int
	TotalPoints int
	Settings    BetSettings
}

func NewBet(outcomes []interface{}, settings BetSettings) *Bet {
	bet := &Bet{
		Outcomes: make([]*Outcome, 0),
		Settings: settings,
	}

	for _, o := range outcomes {
		if oData, ok := o.(map[string]interface{}); ok {
			bet.Outcomes = append(bet.Outcomes, NewOutcomeFromGQL(oData))
		}
	}

	return bet
}

func (b *Bet) UpdateOutcomes(outcomes []interface{}) {
	for i, o := range outcomes {
		if i >= len(b.Outcomes) {
			break
		}
		oData, ok := o.(map[string]interface{})
		if !ok {
			continue
		}

		if users, ok := oData["total_users"].(float64); ok {
			b.Outcomes[i].TotalUsers = int(users)
		}
		if points, ok := oData["total_points"].(float64); ok {
			b.Outcomes[i].TotalPoints = int(points)
		}

		if topPredictors, ok := oData["top_predictors"].([]interface{}); ok && len(topPredictors) > 0 {
			maxPoints := 0
			for _, tp := range topPredictors {
				if pred, ok := tp.(map[string]interface{}); ok {
					if pts, ok := pred["points"].(float64); ok {
						if int(pts) > maxPoints {
							maxPoints = int(pts)
						}
					}
				}
			}
			b.Outcomes[i].TopPoints = maxPoints
		}
	}

	b.TotalUsers = 0
	b.TotalPoints = 0
	for _, o := range b.Outcomes {
		b.TotalUsers += o.TotalUsers
		b.TotalPoints += o.TotalPoints
	}

	if b.TotalUsers > 0 && b.TotalPoints > 0 {
		for _, o := range b.Outcomes {
			o.PercentageUsers = roundFloat((float64(o.TotalUsers)*100)/float64(b.TotalUsers), 2)
			if o.TotalPoints > 0 {
				o.Odds = roundFloat(float64(b.TotalPoints)/float64(o.TotalPoints), 2)
				o.OddsPercentage = roundFloat(100/o.Odds, 2)
			}
		}
	}
}

func (b *Bet) returnChoice(key OutcomeKey) int {
	largest := 0
	for i := 1; i < len(b.Outcomes); i++ {
		if b.getOutcomeValue(i, key) > b.getOutcomeValue(largest, key) {
			largest = i
		}
	}
	return largest
}

func (b *Bet) getOutcomeValue(index int, key OutcomeKey) float64 {
	if index >= len(b.Outcomes) {
		return 0
	}
	o := b.Outcomes[index]
	switch key {
	case OutcomePercentageUsers:
		return o.PercentageUsers
	case OutcomeOddsPercentage:
		return o.OddsPercentage
	case OutcomeOdds:
		return o.Odds
	case OutcomeTopPoints:
		return float64(o.TopPoints)
	case OutcomeTotalUsers:
		return float64(o.TotalUsers)
	case OutcomeTotalPoints:
		return float64(o.TotalPoints)
	default:
		return 0
	}
}

func (b *Bet) returnNumberChoice(number int) int {
	if len(b.Outcomes) > number {
		return number
	}
	return 0
}

func (b *Bet) Skip() (bool, float64) {
	if b.Settings.FilterCondition == nil {
		return false, 0
	}

	fc := b.Settings.FilterCondition
	key := fc.By
	condition := fc.Where
	value := fc.Value

	var comparedValue float64

	fixedKey := key
	if key == OutcomeDecisionUsers || key == OutcomeDecisionPoints {
		if key == OutcomeDecisionUsers {
			fixedKey = OutcomeTotalUsers
		} else {
			fixedKey = OutcomeTotalPoints
		}
	}

	if key == OutcomeTotalUsers || key == OutcomeTotalPoints {
		if len(b.Outcomes) >= 2 {
			comparedValue = b.getOutcomeValue(0, fixedKey) + b.getOutcomeValue(1, fixedKey)
		}
	} else {
		comparedValue = b.getOutcomeValue(b.Decision.Choice, fixedKey)
	}

	switch condition {
	case ConditionGT:
		if comparedValue > value {
			return false, comparedValue
		}
	case ConditionLT:
		if comparedValue < value {
			return false, comparedValue
		}
	case ConditionGTE:
		if comparedValue >= value {
			return false, comparedValue
		}
	case ConditionLTE:
		if comparedValue <= value {
			return false, comparedValue
		}
	}

	return true, comparedValue
}

func (b *Bet) Calculate(balance int) Decision {
	b.Decision = Decision{Choice: -1, Amount: 0, ID: ""}

	switch b.Settings.Strategy {
	case StrategyMostVoted:
		b.Decision.Choice = b.returnChoice(OutcomeTotalUsers)
	case StrategyHighOdds:
		b.Decision.Choice = b.returnChoice(OutcomeOdds)
	case StrategyPercentage:
		b.Decision.Choice = b.returnChoice(OutcomeOddsPercentage)
	case StrategySmartMoney:
		b.Decision.Choice = b.returnChoice(OutcomeTopPoints)
	case StrategyNumber1:
		b.Decision.Choice = b.returnNumberChoice(0)
	case StrategyNumber2:
		b.Decision.Choice = b.returnNumberChoice(1)
	case StrategyNumber3:
		b.Decision.Choice = b.returnNumberChoice(2)
	case StrategyNumber4:
		b.Decision.Choice = b.returnNumberChoice(3)
	case StrategyNumber5:
		b.Decision.Choice = b.returnNumberChoice(4)
	case StrategyNumber6:
		b.Decision.Choice = b.returnNumberChoice(5)
	case StrategyNumber7:
		b.Decision.Choice = b.returnNumberChoice(6)
	case StrategyNumber8:
		b.Decision.Choice = b.returnNumberChoice(7)
	case StrategySmart:
		if len(b.Outcomes) >= 2 {
			difference := math.Abs(b.Outcomes[0].PercentageUsers - b.Outcomes[1].PercentageUsers)
			if difference < float64(b.Settings.PercentageGap) {
				b.Decision.Choice = b.returnChoice(OutcomeOdds)
			} else {
				b.Decision.Choice = b.returnChoice(OutcomeTotalUsers)
			}
		}
	}

	if b.Decision.Choice >= 0 && b.Decision.Choice < len(b.Outcomes) {
		b.Decision.ID = b.Outcomes[b.Decision.Choice].ID

		amount := int(float64(balance) * (float64(b.Settings.Percentage) / 100))
		if amount > b.Settings.MaxPoints {
			amount = b.Settings.MaxPoints
		}

		if b.Settings.StealthMode && amount >= b.Outcomes[b.Decision.Choice].TopPoints {
			reduceAmount := rand.Float64()*4 + 1
			amount = b.Outcomes[b.Decision.Choice].TopPoints - int(reduceAmount)
		}

		b.Decision.Amount = amount
	}

	return b.Decision
}

func (b *Bet) GetDecision() *Outcome {
	if b.Decision.Choice >= 0 && b.Decision.Choice < len(b.Outcomes) {
		return b.Outcomes[b.Decision.Choice]
	}
	return nil
}

func roundFloat(val float64, precision int) float64 {
	ratio := math.Pow(10, float64(precision))
	return math.Round(val*ratio) / ratio
}
