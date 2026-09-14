// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package rewrap is the rewrap role of luxd, the third role of the
// server binary in spec 005: it re-wraps every stored credential's data
// key under the first key of LUX_SECRETS_KEK and exits. The run touches
// the two wrap columns of a row and never the value ciphertext, which is
// never decrypted; a row already under the first key is skipped, a row
// whose version moved during the run is left with its newer wrap, and a
// row no listed key opens is reported by provider id while the run
// continues. The run is idempotent and resumable: an interrupted run is
// repeated rather than repaired. It works over the store.Credentials
// interface, so it runs against any store that holds rows; the role
// requires LUX_DB_URL because only the Postgres store has rows that
// outlive a process.
package rewrap

import (
	"context"
	"errors"
	"fmt"
	"io"

	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
)

// Summary is what one run did, and the one line it prints.
type Summary struct {
	// Rewrapped rows had their data key wrapped under a later key and
	// are now under the first.
	Rewrapped int
	// Current rows were under the first key already, or were re-sealed
	// under it by an apply during the run.
	Current int
	// Unopenable rows opened under no listed key, by provider id, in the
	// store's order.
	Unopenable []string
}

// String is the one line the role prints.
func (s Summary) String() string {
	return fmt.Sprintf("rewrap: %d re-wrapped, %d already current, %d unopenable", s.Rewrapped, s.Current, len(s.Unopenable))
}

// Failed reports whether the run leaves rows the keys cannot open, which
// is the exit code 1 of the role.
func (s Summary) Failed() bool { return len(s.Unopenable) > 0 }

// Run re-wraps every row of creds under the first key of k and reports
// what it did. An unopenable row is written to report as one line naming
// the provider id and the run continues; a store failure ends the run
// with the error and the summary so far.
func Run(ctx context.Context, creds store.Credentials, k *secrets.Keyring, report io.Writer) (Summary, error) {
	var s Summary
	ids, err := creds.List(ctx)
	if err != nil {
		return s, fmt.Errorf("listing the credential rows: %w", err)
	}
	for _, id := range ids {
		row, err := creds.Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue // deleted during the run
		}
		if err != nil {
			return s, fmt.Errorf("reading the credential row of %s: %w", id, err)
		}
		out, changed, err := k.Rewrap(id, row)
		if err != nil {
			s.Unopenable = append(s.Unopenable, id)
			_, _ = fmt.Fprintf(report, "rewrap: provider %s: %v\n", id, err)
			continue
		}
		if !changed {
			s.Current++
			continue
		}
		err = creds.Rewrap(ctx, id, row.Version, out.WrappedKey, out.WrappedNonce)
		switch {
		case errors.Is(err, store.ErrVersionConflict):
			// A value applied during the run was sealed under the first
			// key by the apply, so the stale wrap is dropped and the
			// row counts as current.
			s.Current++
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return s, fmt.Errorf("writing the re-wrapped key of %s: %w", id, err)
		default:
			s.Rewrapped++
		}
	}
	return s, nil
}
