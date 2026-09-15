// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main // import "miniflux.app/v2/sidecar/cmd/mcp"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// TestIsNormalStdioTerminationRecognisesCleanShutdown covers the shapes a
// clean stdin close can actually take coming back from server.Run: a bare
// io.EOF (caught via errors.Is), and the SDK's own "server is closing:
// EOF" text (caught via the ": EOF" suffix check -- see
// isNormalStdioTermination's own doc comment for why a plain errors.Is
// check alone does not catch this second shape).
func TestIsNormalStdioTerminationRecognisesCleanShutdown(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bare io.EOF", io.EOF},
		{"io.EOF wrapped with %w", fmt.Errorf("server run cancelled: %w", io.EOF)},
		{"SDK's own unwrapped text (%v, not %w)", errors.New("server is closing: EOF")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !isNormalStdioTermination(c.err) {
				t.Errorf("isNormalStdioTermination(%v) = false, want true", c.err)
			}
		})
	}
}

// TestIsNormalStdioTerminationRejectsGenuineErrors is the discriminating
// half: a nil error is not a termination at all, context.Canceled and an
// arbitrary transport error are genuine failures, and -- the specific
// trap this function's own doc comment calls out -- io.ErrUnexpectedEOF
// ("unexpected EOF") must NOT be swallowed just because its text also
// ends in the letters "EOF": that is a truncated read, not a clean close.
func TestIsNormalStdioTerminationRejectsGenuineErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"context.Canceled", context.Canceled},
		{"unexpected EOF (truncated read, not a clean close)", io.ErrUnexpectedEOF},
		{"an arbitrary transport error", errors.New("dial tcp 127.0.0.1:8081: connect: connection refused")},
		{"an error that merely contains EOF mid-string", errors.New("EOFless retry exhausted")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if isNormalStdioTermination(c.err) {
				t.Errorf("isNormalStdioTermination(%v) = true, want false", c.err)
			}
		})
	}
}
