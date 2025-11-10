package idempotency

import (
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"time"

	"github.com/google/uuid"
)

func MakeRequest() error {
	const MAX_RETRIES = 3
	const BASE_DELAY = 100 * time.Millisecond
	const MAX_JITTER_MS = 100

	// Create a new random source. Using this is better for concurrency
	// than the global rand.Intn.
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	client := &http.Client{Timeout: 1 * time.Minute}

	// We'll store the last error to return if all retries fail
	var lastErr error
	var response *http.Response

	for i := 0; i <= MAX_RETRIES; i++ {
		// --- 1. Create the request ---
		// We create a new request object for each attempt.
		// This is crucial if the request body was a stream.
		request, err := http.NewRequest("POST", "http://testing.com", nil)
		if err != nil {
			// This is a non-retryable error (e.g., bad URL)
			return fmt.Errorf("error creating the request %v", err)
		}
		request.Header.Add("Idempotency-Key", uuid.NewString())

		// --- 2. Attempt the request ---
		response, err = client.Do(request)
		lastErr = err // Store the error

		// --- 3. Check for success ---
		// Success = no network error AND a non-server-error (non-5xx) status.
		// 4xx errors are client errors and typically not retryable.
		if err == nil && response.StatusCode < 500 {
			slog.Info("Request successful", "status", response.Status)
			if response.Body != nil {
				response.Body.Close() // Good practice to close the body
			}
			return nil // Success!
		}

		// --- 4. Handle retryable failure ---
		// If we're here, it was either a network error (err != nil)
		// or a server error (response.StatusCode >= 500).

		// Close the response body (if it exists) before retrying
		if response != nil && response.Body != nil {
			response.Body.Close()
		}

		// Don't sleep if this was the last attempt
		if i == MAX_RETRIES {
			break
		}

		// --- 5. Calculate backoff and jitter ---

		// Exponential backoff: (base_delay * 2^i)
		backoff := BASE_DELAY * time.Duration(math.Pow(2, float64(i)))

		// Jitter: random duration between 0 and MAX_JITTER_MS
		jitter := time.Duration(r.Intn(MAX_JITTER_MS)) * time.Millisecond

		// Total sleep duration
		sleepDuration := backoff + jitter

		slog.Warn("Request failed, retrying...",
			"attempt", i+1,
			"sleep_duration", sleepDuration.String(),
			"error", err,
		)

		// Wait before the next attempt
		time.Sleep(sleepDuration)
	}

	// If the loop finishes, all retries have failed
	if lastErr != nil {
		return fmt.Errorf("all retries failed, last network error: %v", lastErr)
	}
	// Handle case where the last attempt was a 5xx error
	return fmt.Errorf("all retries failed, last status: %s", response.Status)
}

func HandleRequest(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")

	if len(idempotencyKey) == 0 {
		slog.Error("error processing the request",
			slog.String("key", idempotencyKey),
			slog.String("err", "no idempotencyKey provided"),
		)

		w.WriteHeader(http.StatusBadRequest)
		return

	}

	wasProcessed, err := checkKeyAlreadyProcessed(idempotencyKey)
	if err != nil {
		slog.Error("error checking if key was processed", slog.String("err", err.Error()))

		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !wasProcessed {
		err = process(idempotencyKey)

		if err != nil {
			slog.Error("error processing the request",
				slog.String("key", idempotencyKey),
				slog.String("err", err.Error()),
			)

			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	// Was already processed
	w.WriteHeader(http.StatusCreated)
}

func checkKeyAlreadyProcessed(key string) (bool, error) {
	return false, nil
}

func process(key string) error {
	return nil
}
