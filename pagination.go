package gophercloud

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/mitchellh/mapstructure"
	"github.com/racker/perigee"
)

var (
	// ErrPageNotAvailable is returned from a Pager when a next or previous page is requested, but does not exist.
	ErrPageNotAvailable = errors.New("The requested Collection page does not exist.")
)

// LastHTTPResponse stores generic information derived from an HTTP response.
type LastHTTPResponse struct {
	url.URL
	http.Header
	Body interface{}
}

// RememberHTTPResponse parses an HTTP response as JSON and returns a LastHTTPResponse containing the results.
// The main reason to do this instead of holding the response directly is that a response body can only be read once.
// Also, this centralizes the JSON decoding.
func RememberHTTPResponse(resp http.Response) (LastHTTPResponse, error) {
	var parsedBody interface{}

	defer resp.Body.Close()
	rawBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return LastHTTPResponse{}, err
	}

	if resp.Header.Get("Content-Type") == "application/json" {
		err = json.Unmarshal(rawBody, &parsedBody)
		if err != nil {
			return LastHTTPResponse{}, err
		}
	} else {
		parsedBody = rawBody
	}

	return LastHTTPResponse{
		URL:    *resp.Request.URL,
		Header: resp.Header,
		Body:   parsedBody,
	}, err
}

// request performs a Perigee request and extracts the http.Response from the result.
func request(client *ServiceClient, url string) (http.Response, error) {
	resp, err := perigee.Request("GET", url, perigee.Options{
		MoreHeaders: client.Provider.AuthenticatedHeaders(),
		OkCodes:     []int{200, 204},
	})
	if err != nil {
		return http.Response{}, err
	}
	return resp.HttpResponse, nil
}

// Page must be satisfied by the result type of any resource collection.
// It allows clients to interact with the resource uniformly, regardless of whether or not or how it's paginated.
type Page interface {

	// NextPageURL generates the URL for the page of data that follows this collection.
	// Return "" if no such page exists.
	NextPageURL() (string, error)
}

// SinglePage is a page that contains all of the results from an operation.
type SinglePage LastHTTPResponse

// NextPageURL always returns "" to indicate that there are no more pages to return.
func (current SinglePage) NextPageURL() (string, error) {
	return "", nil
}

// LinkedPage is a page in a collection that provides navigational "Next" and "Previous" links within its result.
type LinkedPage LastHTTPResponse

// NextPageURL extracts the pagination structure from a JSON response and returns the "next" link, if one is present.
func (current LinkedPage) NextPageURL() (string, error) {
	type response struct {
		Links struct {
			Next *string `mapstructure:"next,omitempty"`
		} `mapstructure:"links"`
	}

	var r response
	err := mapstructure.Decode(current.Body, &r)
	if err != nil {
		return "", err
	}

	if r.Links.Next == nil {
		return "", nil
	}

	return *r.Links.Next, nil
}

// MarkerPage is a page in a collection that's paginated by "limit" and "marker" query parameters.
type MarkerPage struct {
	LastHTTPResponse

	// lastMark is a captured function that returns the final entry on a given page.
	lastMark func(Page) (string, error)
}

// NextPageURL generates the URL for the page of results after this one.
func (current MarkerPage) NextPageURL() (string, error) {
	currentURL := current.LastHTTPResponse.URL

	mark, err := current.lastMark(current)
	if err != nil {
		return "", err
	}

	q := currentURL.Query()
	q.Set("marker", mark)
	currentURL.RawQuery = q.Encode()

	return currentURL.String(), nil
}

// Pager knows how to advance through a specific resource collection, one page at a time.
type Pager struct {
	initialURL string

	fetchNextPage func(string) (Page, error)

	countPage func(Page) (int, error)
}

// NewPager constructs a manually-configured pager.
// Supply the URL for the first page, a function that requests a specific page given a URL, and a function that counts a page.
func NewPager(initialURL string, fetchNextPage func(string) (Page, error), countPage func(Page) (int, error)) Pager {
	return Pager{
		initialURL:    initialURL,
		fetchNextPage: fetchNextPage,
		countPage:     countPage,
	}
}

// NewSinglePager constructs a Pager that "iterates" over a single Page.
// Supply the URL to request.
func NewSinglePager(client *ServiceClient, onlyURL string, countPage func(Page) (int, error)) Pager {
	consumed := false
	single := func(_ string) (Page, error) {
		if !consumed {
			consumed = true
			resp, err := request(client, onlyURL)
			if err != nil {
				return SinglePage{}, err
			}

			cp, err := RememberHTTPResponse(resp)
			if err != nil {
				return SinglePage{}, err
			}
			return SinglePage(cp), nil
		}
		return SinglePage{}, ErrPageNotAvailable
	}

	return Pager{
		initialURL:    "",
		fetchNextPage: single,
		countPage:     countPage,
	}
}

// NewLinkedPager creates a Pager that uses a "links" element in the JSON response to locate the next page.
func NewLinkedPager(client *ServiceClient, initialURL string, countPage func(Page) (int, error)) Pager {
	fetchNextPage := func(url string) (Page, error) {
		resp, err := request(client, url)
		if err != nil {
			return nil, err
		}

		cp, err := RememberHTTPResponse(resp)
		if err != nil {
			return nil, err
		}

		return LinkedPage(cp), nil
	}

	return Pager{
		initialURL:    initialURL,
		fetchNextPage: fetchNextPage,
		countPage:     countPage,
	}
}

// NewMarkerPager creates a Pager that iterates over successive pages by issuing requests with a "marker" parameter set to the
// final element of the previous Page.
func NewMarkerPager(client *ServiceClient, initialURL string,
	lastMark func(Page) (string, error), countPage func(Page) (int, error)) Pager {

	fetchNextPage := func(currentURL string) (Page, error) {
		resp, err := request(client, currentURL)
		if err != nil {
			return nil, err
		}

		last, err := RememberHTTPResponse(resp)
		if err != nil {
			return nil, err
		}

		return MarkerPage{LastHTTPResponse: last, lastMark: lastMark}, nil
	}

	return Pager{
		initialURL:    initialURL,
		fetchNextPage: fetchNextPage,
		countPage:     countPage,
	}
}

// EachPage iterates over each page returned by a Pager, yielding one at a time to a handler function.
// Return "false" from the handler to prematurely stop iterating.
func (p Pager) EachPage(handler func(Page) (bool, error)) error {
	currentURL := p.initialURL
	for {
		currentPage, err := p.fetchNextPage(currentURL)
		if err != nil {
			return err
		}

		count, err := p.countPage(currentPage)
		if err != nil {
			return err
		}
		if count == 0 {
			return nil
		}

		ok, err := handler(currentPage)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}

		currentURL, err = currentPage.NextPageURL()
		if err != nil {
			return err
		}
		if currentURL == "" {
			return nil
		}
	}
}
