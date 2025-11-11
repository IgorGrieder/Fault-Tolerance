package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Define the states for the circuit breaker
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

// Sentinel error
var ErrCircuitOpen = errors.New("circuit breaker is open")

type CircuitBreaker struct {
	mu          sync.Mutex
	state       State
	failures    int
	maxFailures int
	openSince   time.Time
	openTimeout time.Duration
}

// NewCircuitBreaker creates a new circuit breaker with its thresholds
func NewCircuitBreaker(maxFailures int, openTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:       StateClosed,
		maxFailures: maxFailures,
		openTimeout: openTimeout,
	}
}

// Handler will hold our client and the circuit breaker
type Handler struct {
	client *http.Client
	cb     *CircuitBreaker
}

// NewHandler creates a new handler
func NewHandler(cb *CircuitBreaker) *Handler {
	return &Handler{
		client: &http.Client{Timeout: 1 * time.Minute},
		cb:     cb,
	}
}

// BeforeRequest checks if a request is allowed to proceed
func (cb *CircuitBreaker) BeforeRequest() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		// Always allowed in a closed state
		return nil

	case StateOpen:
		// Check if the open timeout has elapsed
		if time.Since(cb.openSince) > cb.openTimeout {
			// Timeout exceeded -> Half-Open
			slog.Warn("Circuit Breaker: Open -> Half-Open")
			cb.state = StateHalfOpen
			return nil // Allow one test request to go through
		}

		// Still open
		return ErrCircuitOpen

	case StateHalfOpen:
		// The circuit is already in a Half-Open state,
		// meaning a test request is in flight.
		// Deny all other concurrent requests.
		return ErrCircuitOpen
	}
	return nil
}

// OnSuccess notifies the breaker of a successful call
func (cb *CircuitBreaker) OnSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateHalfOpen:
		// Test request succeeded -> close circuit
		slog.Info("Circuit Breaker: Half-Open -> Closed")
		cb.state = StateClosed
		cb.failures = 0

	case StateClosed:
		// Reset consecutive failures
		cb.failures = 0
	}
}

// OnFailure notifies the breaker of a failed call
func (cb *CircuitBreaker) OnFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateHalfOpen:
		// The test request failed -> go into open state again
		slog.Error("Circuit Breaker: Half-Open -> Open (test failed)")
		cb.state = StateOpen
		cb.openSince = time.Now() // Reset the open timer

	case StateClosed:
		cb.failures++
		slog.Warn("Circuit Breaker: Failure recorded", "count", cb.failures)

		// Check if we've reached the threshold
		if cb.failures >= cb.maxFailures {
			slog.Error("Circuit Breaker: Closed -> Open (threshold reached)")
			cb.state = StateOpen
			cb.openSince = time.Now()
		}
	}
}

// MakeRequest is the new public-facing method.
// It wraps the retry logic with the circuit breaker check.
func (h *Handler) MakeRequest() error {
	// --- 1. Check Circuit Breaker ---
	if err := h.cb.BeforeRequest(); err != nil {
		// Circuit is Open or Half-Open (and busy), so we fast-fail.
		slog.Error("Request blocked by circuit breaker", "error", err)
		return err
	}

	// --- 2. Attempt the operation (with retries) ---
	// The circuit breaker allows the request
	err := h.attemptRequestWithRetry()

	// --- 3. Report the outcome ---
	if err != nil {
		// The operation failed *after* all retries
		h.cb.OnFailure()
	} else {
		// The operation succeeded
		h.cb.OnSuccess()
	}

	return err
}

// attemptRequestWithRetry is your original function, now a method on Handler
func (h *Handler) attemptRequestWithRetry() error {
	const MAX_RETRIES = 3
	const BASE_DELAY = 100 * time.Millisecond
	const MAX_JITTER_MS = 100

	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	var lastErr error
	var response *http.Response

	for i := 0; i <= MAX_RETRIES; i++ {
		request, err := http.NewRequest("POST", "http://testing.com", nil)
		if err != nil {
			return fmt.Errorf("error creating the request %v", err)
		}
		request.Header.Add("Idempotency-Key", uuid.NewString())

		// Use the handler's client
		response, err = h.client.Do(request)
		lastErr = err

		if err == nil && response.StatusCode < 500 {
			slog.Info("Request successful", "status", response.Status)
			if response.Body != nil {
				response.Body.Close()
			}
			return nil // Success!
		}

		// Close body (if any) before retry
		if response != nil && response.Body != nil {
			response.Body.Close()
		}

		if i == MAX_RETRIES {
			break
		}

		// Calculate backoff and jitter
		backoff := BASE_DELAY * time.Duration(math.Pow(2, float64(i)))
		jitter := time.Duration(r.Intn(MAX_JITTER_MS)) * time.Millisecond
		sleepDuration := backoff + jitter

		// Safely create error message
		errMsg := "server error"
		if err != nil {
			errMsg = err.Error()
		}

		slog.Warn("Request failed, retrying...",
			"attempt", i+1,
			"sleep_duration", sleepDuration.String(),
			"error", errMsg,
		)

		time.Sleep(sleepDuration)
	}

	// All retries failed
	if lastErr != nil {
		return fmt.Errorf("all retries failed, last network error: %v", lastErr)
	}

	return fmt.Errorf("all retries failed, last status: %s", response.Status)
}
