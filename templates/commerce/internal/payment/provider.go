package payment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
)

// FailureCode classifies a hard provider decline. The schema's CHECK constraint
// and order's contract require these exact values.
type FailureCode string

const (
	FailureInsufficientFunds FailureCode = "insufficient_funds"
	FailureCardDeclined     FailureCode = "card_declined"
	FailureProviderTimeout  FailureCode = "provider_timeout"
	FailureRiskRejection    FailureCode = "risk_rejection"
)

// Scenario selects a deterministic provider outcome stream for an intent. The
// simulator never consults wall-clock randomness; a fixed scenario plus the
// intent identity reproduce the same attempt outcomes on every replay.
type Scenario string

const (
	// ScenarioProviderTimeoutThenSuccess yields one transient provider_timeout
	// followed by a successful capture. Matches the Task 7 contract:
	// transient payment → 2 attempts → success.
	ScenarioProviderTimeoutThenSuccess Scenario = "provider_timeout_then_success"
	// ScenarioCardDeclined is a hard decline: every attempt fails with
	// card_declined.
	ScenarioCardDeclined Scenario = "card_declined"
	// ScenarioInsufficientFunds fails every attempt with insufficient_funds.
	ScenarioInsufficientFunds Scenario = "insufficient_funds"
	// ScenarioRiskRejection fails every attempt with risk_rejection.
	ScenarioRiskRejection Scenario = "risk_rejection"
)

// ErrInvalidScenario is returned when a Scenario value is not recognized.
var ErrInvalidScenario = errors.New("invalid payment scenario")

// ChargeRequest is the deterministic input to the provider simulator.
type ChargeRequest struct {
	IntentID        string
	IdempotencyKey  string
	OrderID         string
	Currency        string
	AmountMinor     int64
	Attempt         int // 1-based attempt number for this intent
	MethodToken     string
	ProviderEventID string // deterministic, replay-safe webhook identifier
}

// ChargeResult is the provider's verdict for a single attempt.
type ChargeResult struct {
	ProviderReference string
	Status            string // "succeeded" or "failed"
	FailureCode       FailureCode
	ProviderEventID   string
}

// Provider simulates a payment acquirer. Implementations must be deterministic
// in their inputs so that replays of the same order produce identical state.
type Provider interface {
	Charge(request ChargeRequest) (ChargeResult, error)
	// MaxAttempts is the deterministic attempt budget for this provider's
	// configured scenario. Transient scenarios retry once before success;
	// hard declines do not retry.
	MaxAttempts() int
}

// Simulator is the deterministic in-process provider. Outcomes derive solely
// from the configured scenario and the attempt number; replay safety follows
// from the intent's deterministic identities.
type Simulator struct {
	scenario Scenario
	mu       sync.Mutex
}

// NewSimulator returns a deterministic provider bound to scenario.
func NewSimulator(scenario Scenario) *Simulator {
	return &Simulator{scenario: scenario}
}

// Charge derives the provider outcome for request.
func (simulator *Simulator) Charge(request ChargeRequest) (ChargeResult, error) {
	if simulator == nil {
		return ChargeResult{}, errors.New("payment provider is nil")
	}
	if request.Attempt <= 0 {
		return ChargeResult{}, errors.New("payment attempt must be >= 1")
	}
	if request.ProviderEventID == "" {
		request.ProviderEventID = deterministicProviderEventID(request.IntentID, request.Attempt)
	}
	reference := deterministicProviderReference(request.IntentID)
	simulator.mu.Lock()
	scenario := simulator.scenario
	simulator.mu.Unlock()
	switch scenario {
	case ScenarioProviderTimeoutThenSuccess:
		if request.Attempt == 1 {
			return ChargeResult{
				ProviderReference: reference, Status: "failed",
				FailureCode: FailureProviderTimeout, ProviderEventID: request.ProviderEventID,
			}, nil
		}
		return ChargeResult{
			ProviderReference: reference, Status: "succeeded",
			ProviderEventID: request.ProviderEventID,
		}, nil
	case ScenarioCardDeclined:
		return ChargeResult{
			ProviderReference: reference, Status: "failed",
			FailureCode: FailureCardDeclined, ProviderEventID: request.ProviderEventID,
		}, nil
	case ScenarioInsufficientFunds:
		return ChargeResult{
			ProviderReference: reference, Status: "failed",
			FailureCode: FailureInsufficientFunds, ProviderEventID: request.ProviderEventID,
		}, nil
	case ScenarioRiskRejection:
		return ChargeResult{
			ProviderReference: reference, Status: "failed",
			FailureCode: FailureRiskRejection, ProviderEventID: request.ProviderEventID,
		}, nil
	}
	return ChargeResult{}, fmt.Errorf("%w: %q", ErrInvalidScenario, scenario)
}

// MaxAttempts returns the deterministic attempt budget for the simulator's
// configured scenario.
func (simulator *Simulator) MaxAttempts() int {
	if simulator == nil {
		return 1
	}
	simulator.mu.Lock()
	defer simulator.mu.Unlock()
	return MaxAttemptsFor(simulator.scenario)
}

// MaxAttemptsFor returns the deterministic attempt budget for scenario.
// Transient failures retry once before success; hard declines do not retry.
func MaxAttemptsFor(scenario Scenario) int {
	switch scenario {
	case ScenarioProviderTimeoutThenSuccess:
		return 2
	default:
		return 1
	}
}

// deterministicProviderReference derives a stable acquirer reference so that
// replays converge on the same UNIQUE payment_intent.provider_reference value.
func deterministicProviderReference(intentID string) string {
	if intentID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("provider_ref\x00" + intentID))
	return "pr_" + hex.EncodeToString(sum[:12])
}

// deterministicProviderEventID derives a stable webhook identifier per attempt.
func deterministicProviderEventID(intentID string, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("provider_event\x00%s\x00%d", intentID, attempt)))
	return "pev_" + hex.EncodeToString(sum[:12])
}

var _ Provider = (*Simulator)(nil)
