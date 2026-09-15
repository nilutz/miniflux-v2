// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model // import "miniflux.app/v2/internal/model"

// EntryHiddenReasonBulk marks a hidden entry as the result of a mass action —
// the "hide existing articles" backlog checkbox on subscribe, or a
// mark-all-as-hidden route — rather than a human judging one entry by hand.
//
// Spec §13.3: only a hand-driven hide is a real negative preference signal
// for goal 3's daily best-of. The Go zero value for hidden_reason (NULL in
// the database, "" here) means hand-driven; this constant means "the reader
// never looked at these."
const EntryHiddenReasonBulk = "bulk"
