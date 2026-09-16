// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package indexer // import "miniflux.app/v2/sidecar/internal/indexer"

import (
	"context"
	"testing"
	"time"
)

// TestIndexerCloseWaitsForInFlightEmbedBeforeClosingEmbedder is the
// deterministic reproduction of the process-shutdown use-after-free
// defect 1 describes: a confirmed segfault where goroutine 1 ran
// ReleaseOrtSession on the same session pointer a worker goroutine was
// still inside RunOrtSessionWithOptions on, reproducing on every
// container restart.
//
// It blocks an in-flight embed call to hold embedderMu's read lock (via
// AsEmbedder, the same accessor every production caller -- the indexing
// lanes and the search HTTP handlers alike -- uses), calls Close from
// another goroutine, and asserts Close neither returns nor actually
// closes the embedder while that call is still in flight; only once the
// call is unblocked and returns does Close proceed. Against the prior
// implementation (idx.Embedder().Close() -- a momentary RLock to copy the
// pointer, released before Close() is ever called on it) this fails
// immediately: Close returns and closes the embedder well before the
// in-flight call is unblocked, exactly the race the crash report showed.
func TestIndexerCloseWaitsForInFlightEmbedBeforeClosingEmbedder(t *testing.T) {
	e := &switchBlockingEmbedder{
		identity: "model-a@rev1#768",
		blocked:  make(chan struct{}),
		unblock:  make(chan struct{}),
	}
	idx := New(nil, e)

	embedDone := make(chan error, 1)
	go func() {
		_, err := idx.AsEmbedder().EmbedDocuments(context.Background(), []string{"hello"})
		embedDone <- err
	}()

	select {
	case <-e.blocked:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the in-flight embed call to start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- idx.Close() }()

	// Close must not have returned, and must not have closed the
	// embedder, while the embed call above is still blocked in flight.
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned (err=%v) while an embed call was still in flight -- the use-after-free defect 1 describes", err)
	case <-time.After(200 * time.Millisecond):
	}
	if e.closed.Load() {
		t.Fatalf("embedder was closed while an embed call was still in flight")
	}

	close(e.unblock)

	select {
	case err := <-embedDone:
		if err != nil {
			t.Fatalf("EmbedDocuments failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("in-flight embed call did not complete after being unblocked")
	}

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Close did not return after the in-flight embed call finished")
	}
	if !e.closed.Load() {
		t.Fatalf("expected Close to close the embedder once the in-flight call finished")
	}
}

// TestIndexerCloseOnNilEmbedderIsANoOp covers the config-only Backfill
// construction New's own doc comment describes (e may be nil), so Close
// on such an Indexer must not panic dereferencing a nil embedder.
func TestIndexerCloseOnNilEmbedderIsANoOp(t *testing.T) {
	idx := New(nil, nil)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close on a nil embedder returned an error: %v", err)
	}
}
