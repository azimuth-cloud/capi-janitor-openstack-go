// Package pageutil validates pages returned by OpenStack list APIs.
package pageutil

import (
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2/pagination"
)

// NewValidatedCollectionPage adds response validation to a Gophercloud page.
func NewValidatedCollectionPage(page pagination.Page, collectionKey string, statusCode int) pagination.Page {
	return validatedCollectionPage{
		Page:          page,
		collectionKey: collectionKey,
		statusCode:    statusCode,
	}
}

// UnwrapCollectionPage returns the original Gophercloud page.
func UnwrapCollectionPage(page pagination.Page) pagination.Page {
	if validatedPage, ok := page.(validatedCollectionPage); ok {
		return validatedPage.Page
	}
	return page
}

type validatedCollectionPage struct {
	pagination.Page
	collectionKey string
	statusCode    int
}

func (p validatedCollectionPage) IsEmpty() (bool, error) {
	if p.statusCode == http.StatusNoContent {
		return true, nil
	}
	if p.statusCode != http.StatusOK {
		return true, fmt.Errorf(
			"validating %q collection response: unexpected HTTP status %d",
			p.collectionKey,
			p.statusCode,
		)
	}

	body, ok := p.GetBody().(map[string]any)
	if !ok {
		return true, fmt.Errorf(
			"validating %q collection envelope: response body must be an object, got %T",
			p.collectionKey,
			p.GetBody(),
		)
	}

	collection, found := body[p.collectionKey]
	if !found {
		return true, fmt.Errorf(
			"validating %q collection envelope: collection key is missing",
			p.collectionKey,
		)
	}
	if collection == nil {
		return true, fmt.Errorf(
			"validating %q collection envelope: collection is null",
			p.collectionKey,
		)
	}
	if _, ok := collection.([]any); !ok {
		return true, fmt.Errorf(
			"validating %q collection envelope: collection must be an array, got %T",
			p.collectionKey,
			collection,
		)
	}

	empty, err := p.Page.IsEmpty()
	if err != nil || !empty {
		return empty, err
	}

	// Keep going when an empty page points to another page.
	nextPageURL, err := p.Page.NextPageURL()
	if err != nil {
		return true, err
	}
	return nextPageURL == "", nil
}
