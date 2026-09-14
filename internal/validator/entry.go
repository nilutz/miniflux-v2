// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package validator // import "miniflux.app/v2/internal/validator"

import (
	"errors"
	"fmt"

	"miniflux.app/v2/internal/model"
)

// ValidateEntriesStatusUpdateRequest validates a status update for a list of entries.
func ValidateEntriesStatusUpdateRequest(request *model.EntriesStatusUpdateRequest) error {
	if len(request.EntryIDs) == 0 {
		return errors.New(`the list of entries cannot be empty`)
	}

	return ValidateEntryStatus(request.Status)
}

// ValidateEntriesStatusAndStarredUpdateRequest validates a status, starred
// and/or hidden update for a list of entries. At least one of the status,
// starred or hidden fields must be specified. This is used by the API, which
// can update the read status, the starred state, the hidden state, or any
// combination of them.
func ValidateEntriesStatusAndStarredUpdateRequest(request *model.EntriesStatusUpdateRequest) error {
	if len(request.EntryIDs) == 0 {
		return errors.New(`the list of entries cannot be empty`)
	}

	if request.Status == "" && request.Starred == nil && request.Hidden == nil {
		return errors.New(`the status, starred or hidden field must be specified`)
	}

	if request.Status != "" {
		return ValidateEntryStatus(request.Status)
	}

	return nil
}

// ValidateEntryStatus makes sure the entry status is valid.
func ValidateEntryStatus(status string) error {
	switch status {
	case model.EntryStatusRead, model.EntryStatusUnread:
		return nil
	}

	return fmt.Errorf(`invalid entry status, valid status values are: %q and %q`, model.EntryStatusRead, model.EntryStatusUnread)
}

// ValidateEntryOrder makes sure the sorting order is valid.
func ValidateEntryOrder(order string) error {
	switch order {
	case "id", "status", "changed_at", "published_at", "created_at", "category_title", "category_id", "title", "author":
		return nil
	}

	return errors.New(`invalid entry order, valid order values are: "id", "status", "changed_at", "published_at", "created_at", "category_title", "category_id", "title", "author"`)
}

// ValidateEntryModification makes sure the entry modification is valid.
func ValidateEntryModification(request *model.EntryUpdateRequest) error {
	if request.Title != nil && *request.Title == "" {
		return errors.New(`the entry title cannot be empty`)
	}

	if request.Content != nil && *request.Content == "" {
		return errors.New(`the entry content cannot be empty`)
	}

	return nil
}
