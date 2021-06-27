package keyfunc

import (
	"bytes"
	"context"
	"io/ioutil"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

var (

	// defaultRefreshTimeout is the default duration for the context used to create the HTTP request for a refresh of
	// the JWKs.
	defaultRefreshTimeout = time.Minute
)

// Get loads the JWKs at the given URL.
func Get(jwksURL string, options ...Options) (jwks *JWKs, err error) {

	// Create the JWKs.
	jwks = &JWKs{
		jwksURL: jwksURL,
	}

	// Apply the options to the JWKs.
	for _, opts := range options {
		applyOptions(jwks, opts)
	}

	// Apply some defaults if options were not provided.
	if jwks.client == nil {
		jwks.client = http.DefaultClient
	}
	if jwks.refreshTimeout == nil {
		jwks.refreshTimeout = &defaultRefreshTimeout
	}

	// Get the keys for the JWKs.
	if err = jwks.refresh(); err != nil {
		return nil, err
	}

	// Check to see if a background refresh of the JWKs should happen.
	if jwks.refreshInterval != nil || jwks.refreshRateLimit != nil || jwks.refreshUnknownKID {

		// Attach a context used to end the background goroutine.
		jwks.ctx, jwks.cancel = context.WithCancel(context.Background())

		// Create a channel that will accept requests to refresh the JWKs.
		jwks.refreshRequests = make(chan context.CancelFunc)

		// Start the background goroutine for data refresh.
		go jwks.backgroundRefresh()
	}

	return jwks, nil
}

// backgroundRefresh is meant to be a separate goroutine that will update the keys in a JWKs over a given interval of
// time.
func (j *JWKs) backgroundRefresh() {

	// Create the rate limiter.
	var limiter *rate.Limiter
	if j.refreshRateLimit != nil {
		every := rate.Every(*j.refreshRateLimit)
		limiter = rate.NewLimiter(every, 1)
	}

	// Create a channel that will never send anything unless there is a refresh interval.
	refreshInterval := make(<-chan time.Time)

	// Enter an infinite loop that ends when the background ends.
	for {

		// If there is a refresh interval, create the channel for it.
		if j.refreshInterval != nil {
			refreshInterval = time.After(*j.refreshInterval)
		}

		// Wait for a refresh to occur or the background to end.
		select {

		// Send a refresh request the JWKs after the given interval.
		case <-refreshInterval:
			select {
			case <-j.ctx.Done():
				return
			case j.refreshRequests <- func() {}:
			}

		// Accept refresh requests.
		case cancel := <-j.refreshRequests:

			// Rate limit, if needed.
			if limiter != nil && !limiter.Allow() {

				// Don't make the JWT parsing goroutine wait for the JWKs to refresh.
				cancel()

				// Launch a goroutine that will get a reservation for a JWKs refresh or fail to and immediately return.
				go func() {

					// Create a reservation to refresh the JWKs.
					reservation := limiter.Reserve()

					// If there's already a reservation, ignore the refresh request.
					if !reservation.OK() {
						return
					}

					// Wait for the reservation to be ready or the context to end.
					select {
					case <-j.ctx.Done():
						return
					case <-time.After(reservation.Delay()):
					}

					// Refresh the JWKs.
					if err := j.refresh(); err != nil && j.refreshErrorHandler != nil {
						j.refreshErrorHandler(err)
					}
				}()
			} else {

				// Refresh the JWKs.
				if err := j.refresh(); err != nil && j.refreshErrorHandler != nil {
					j.refreshErrorHandler(err)
				}
				cancel()
			}

		// Clean up this goroutine when its context expires.
		case <-j.ctx.Done():
			return
		}
	}
}

// refresh does an HTTP GET on the JWKs URL to rebuild the JWKs.
func (j *JWKs) refresh() (err error) {

	// Create a context for the request.
	var ctx context.Context
	var cancel context.CancelFunc
	if j.ctx != nil {
		ctx, cancel = context.WithTimeout(j.ctx, *j.refreshTimeout)
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), *j.refreshTimeout)
	}
	defer cancel()

	// Create the HTTP request.
	var req *http.Request
	if req, err = http.NewRequestWithContext(ctx, http.MethodGet, j.jwksURL, bytes.NewReader(nil)); err != nil {
		return err
	}

	// Get the JWKs as JSON from the given URL.
	var resp *http.Response
	if resp, err = j.client.Do(req); err != nil {
		return err
	}
	defer resp.Body.Close() // Ignore any error.

	// Read the raw JWKs from the body of the response.
	var jwksBytes []byte
	if jwksBytes, err = ioutil.ReadAll(resp.Body); err != nil {
		return err
	}

	// Create an updated JWKs.
	var updated *JWKs
	if updated, err = New(jwksBytes); err != nil {
		return err
	}

	// Lock the JWKs for async safe usage.
	j.mux.Lock()
	defer j.mux.Unlock()

	// Update the keys.
	j.Keys = updated.Keys

	return nil
}
