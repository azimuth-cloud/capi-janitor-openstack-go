package pageutil

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/v2/pagination"
)

func TestValidatedCollectionPageFollowsSparseIntermediatePage(t *testing.T) {
	t.Parallel()

	page := NewValidatedCollectionPage(fakePage{
		body:    map[string]any{"resources": []any{}},
		empty:   true,
		nextURL: "https://openstack.example.test/resources?page=2",
	}, "resources", 200)

	empty, err := page.IsEmpty()
	if err != nil {
		t.Fatalf("checking page: %v", err)
	}
	if empty {
		t.Fatal("expected an empty page with a next link to continue pagination")
	}
}

func TestValidatedCollectionPageStopsAtFinalEmptyPage(t *testing.T) {
	t.Parallel()

	page := NewValidatedCollectionPage(fakePage{
		body:  map[string]any{"resources": []any{}},
		empty: true,
	}, "resources", 200)

	empty, err := page.IsEmpty()
	if err != nil {
		t.Fatalf("checking page: %v", err)
	}
	if !empty {
		t.Fatal("expected final empty page to stop pagination")
	}
}

func TestValidatedCollectionPageRejectsMultipleChoices(t *testing.T) {
	t.Parallel()

	page := NewValidatedCollectionPage(fakePage{
		body:  map[string]any{"resources": []any{}},
		empty: true,
	}, "resources", http.StatusMultipleChoices)

	if _, err := page.IsEmpty(); err == nil {
		t.Fatal("expected HTTP 300 collection response to fail closed")
	}
}

func TestValidatedCollectionPagePropagatesNextLinkError(t *testing.T) {
	t.Parallel()

	nextErr := errors.New("malformed next link")
	page := NewValidatedCollectionPage(fakePage{
		body:    map[string]any{"resources": []any{}},
		empty:   true,
		nextErr: nextErr,
	}, "resources", 200)

	_, err := page.IsEmpty()
	if !errors.Is(err, nextErr) {
		t.Fatalf("expected next-link cause, got %v", err)
	}
}

type fakePage struct {
	body     any
	empty    bool
	emptyErr error
	nextURL  string
	nextErr  error
}

func (p fakePage) NextPageURL() (string, error) {
	return p.nextURL, p.nextErr
}

func (p fakePage) IsEmpty() (bool, error) {
	return p.empty, p.emptyErr
}

func (p fakePage) GetBody() any {
	return p.body
}

var _ pagination.Page = fakePage{}
