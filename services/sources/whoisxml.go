// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package sources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OWASP/Amass/config"
	eb "github.com/OWASP/Amass/eventbus"
	"github.com/OWASP/Amass/net/http"
	"github.com/OWASP/Amass/requests"
	"github.com/OWASP/Amass/resolvers"
	"github.com/OWASP/Amass/services"
)

// WhoisXML is the Service that handles access to the WhoisXML data source.
type WhoisXML struct {
	services.BaseService

	API        *config.APIKey
	SourceType string
	RateLimit  time.Duration
}

//WhoisXMLResponse handles WhoisXML response json
type WhoisXMLResponse struct {
	Found int      `json:"domainsCount"`
	List  []string `json:"domainsList"`
}

//WhoisXMLAdvanceSearchTerms are variables for the api's query with specific fields in mind
type WhoisXMLAdvanceSearchTerms struct {
	Field string `json:"field"`
	Term  string `json:"term"`
}

//WhoisXMLAdvanceRequest handles POST request Json with specific fields.
type WhoisXMLAdvanceRequest struct {
	Search      string                       `json:"searchType"`
	Mode        string                       `json:"mode"`
	SearchTerms []WhoisXMLAdvanceSearchTerms `json:"advancedSearchTerms"`
}

//WhoisXMLBasicSearchTerms for searching by domain
type WhoisXMLBasicSearchTerms struct {
	Include []string `json:"include"`
}

//WhoisXMLBasicRequest is for using general search terms such as including domains and excluding regions
type WhoisXMLBasicRequest struct {
	Search      string                   `json:"searchType"`
	Mode        string                   `json:"mode"`
	SearchTerms WhoisXMLBasicSearchTerms `json:"basicSearchTerms"`
}

// NewWhoisXML returns he object initialized, but not yet started.
func NewWhoisXML(cfg *config.Config, bus *eb.EventBus, pool *resolvers.ResolverPool) *WhoisXML {
	w := &WhoisXML{
		SourceType: requests.API,
		RateLimit:  10 * time.Second,
	}

	w.BaseService = *services.NewBaseService(w, "WhoisXML", cfg, bus, pool)
	return w
}

// OnStart implements the Service interface
func (w *WhoisXML) OnStart() error {
	w.BaseService.OnStart()
	w.API = w.Config().GetAPIKey(w.String())
	if w.API == nil || w.API.Key == "" {
		w.Bus().Publish(requests.LogTopic,
			fmt.Sprintf("%s: API key data was not provided", w.String()),
		)

	}
	w.Bus().Subscribe(requests.WhoisRequestTopic, w.SendWhoisRequest)

	go w.processRequests()
	return nil
}

func (w *WhoisXML) processRequests() {
	last := time.Now().Truncate(10 * time.Minute)

	for {
		select {
		case <-w.Quit():
			return

		case whois := <-w.WhoisRequestChan():
			if w.Config().IsDomainInScope(whois.Domain) {
				if time.Now().Sub(last) < w.RateLimit {
					time.Sleep(w.RateLimit)
				}
				last = time.Now()
				w.executeWhoisQuery(whois.Domain)
				last = time.Now()
			}
		case <-w.AddrRequestChan():
		case <-w.ASNRequestChan():
		}
	}
}

func (w *WhoisXML) executeWhoisQuery(domain string) {
	u := w.getReverseWhoisURL(domain)
	if w.API == nil || w.API.Key == "" {
		w.Bus().Publish(requests.LogTopic,
			fmt.Sprintf("%s: API key data was not provided", w.String()),
		)
		return
	}
	headers := map[string]string{"X-Authentication-Token": w.API.Key}

	var r = WhoisXMLBasicRequest{
		Search: "historic",
		Mode:   "purchase",
	}
	r.SearchTerms.Include = append(r.SearchTerms.Include, domain)
	jr, _ := json.Marshal(r)

	page, err := http.RequestWebPage(u, bytes.NewReader(jr), headers, "", "")
	if err != nil {
		w.Bus().Publish(requests.LogTopic, fmt.Sprintf("%s: %s: %w", w.String(), u, err))
		return
	}

	// Pull the table we need from the page content
	var q WhoisXMLResponse

	err = json.NewDecoder(strings.NewReader(page)).Decode(&q)
	if err != nil {
		w.Bus().Publish(requests.LogTopic,
			fmt.Sprintf("Failed to decode json in WhoisXML.\nErr:%s", err))
		return
	}
	if q.Found > 0 {

		w.Bus().Publish(requests.NewWhoisTopic, &requests.WhoisRequest{
			Domain:     domain,
			NewDomains: q.List,
			Tag:        w.SourceType,
			Source:     w.String(),
		})
	}
}

func (w *WhoisXML) getReverseWhoisURL(domain string) string {
	format := "https://reverse-whois-api.whoisxmlapi.com/api/v2"
	return fmt.Sprintf(format)
}
