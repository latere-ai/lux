// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"latere.ai/x/pkg/otel"

	v1 "latere.ai/x/lux/manifest/v1"
)

// DeliveryDeadline bounds one POST to the sink.
const DeliveryDeadline = 10 * time.Second

// SinkOptions is what the Sink posts under.
type SinkOptions struct {
	// URL is LUX_EVENTS_URL; Secret is LUX_EVENTS_SECRET.
	URL    string
	Secret []byte
	// Client sends every POST; nil is an instrumented client over the
	// default transport. Redirects are never followed on either: a 3xx
	// is a failure.
	Client *http.Client
	// Deadline bounds one POST; zero is DeliveryDeadline.
	Deadline time.Duration
	// Now is the clock the signature's t is read from; nil is time.Now.
	Now func() time.Time
}

// Sink is the operator's endpoint as this process reaches it.
type Sink struct {
	o      SinkOptions
	client http.Client
}

// NewSink constructs the Sink. It sends nothing.
func NewSink(o SinkOptions) *Sink {
	if o.Client == nil {
		o.Client = &http.Client{Transport: otel.Transport(nil)}
	}
	if o.Deadline <= 0 {
		o.Deadline = DeliveryDeadline
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	// A copy, so the caller's client keeps its own redirect policy.
	client := *o.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Sink{o: o, client: client}
}

// DeliveryError is one attempt the sink did not acknowledge: the status
// it answered, or the transport failure.
type DeliveryError struct {
	Status int
	Err    error
}

func (e *DeliveryError) Error() string {
	if e.Err != nil {
		return "POST: " + e.Err.Error()
	}
	return "status " + strconv.Itoa(e.Status)
}

func (e *DeliveryError) Unwrap() error { return e.Err }

// Deliver posts body as one event, signed at this attempt's clock, and
// returns nil on a 2xx. Any other status, a redirect among them, or a
// transport failure is a *DeliveryError. The POST ends at the deadline.
func (s *Sink) Deliver(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s.o.Deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.o.URL, bytes.NewReader(body))
	if err != nil {
		return &DeliveryError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(Header, Sign(s.o.Secret, s.o.Now(), body))
	resp, err := s.client.Do(req)
	if err != nil {
		return &DeliveryError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &DeliveryError{Status: resp.StatusCode}
	}
	return nil
}

// Ping delivers one check.ping, the row luxd check sends to verify the
// sink: it names no object, carries an empty data, has reason check,
// and is never journalled.
func (s *Sink) Ping(ctx context.Context) error {
	now := s.o.Now()
	body, err := encode(Record{ID: v1.NewID(v1.PrefixEvent, now, nil), Type: CheckPing, Time: now.UTC(), Reason: ReasonCheck})
	if err != nil {
		return err
	}
	return s.Deliver(ctx, body)
}
