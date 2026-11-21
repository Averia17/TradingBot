package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const maxResponse = 24 << 20

var (
	accessionPattern = regexp.MustCompile(`^[0-9]{10}-[0-9]{2}-[0-9]{6}$`)
	documentPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	hrefPattern      = regexp.MustCompile(`(?is)href\s*=\s*["']([^"']+)["']`)
	tableRows        = regexp.MustCompile(`(?is)<tr\b[^>]*>.*?</tr>`)
	exhibitType      = regexp.MustCompile(`(?i)\bEX-99\.1\b`)
	activeHTML       = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(?:script|style)>`)
	markup           = regexp.MustCompile(`(?s)<[^>]*>`)
)

type collector struct {
	client      *http.Client
	userAgent   string
	dataRoot    string
	archiveRoot string
	now         func() time.Time
	lastRequest time.Time
}

type recentFilings struct {
	Accessions []string `json:"accessionNumber"`
	Accepted   []string `json:"acceptanceDateTime"`
	Forms      []string `json:"form"`
	Documents  []string `json:"primaryDocument"`
	Items      []string `json:"items"`
	ReportDate []string `json:"reportDate"`
}

type filing struct {
	accession, form, document, items, period string
	accepted                                 time.Time
}

func (c *collector) get(ctx context.Context, endpoint string) ([]byte, error) {
	// SEC's public access policy caps automated requests at ten per second.
	if delay := time.Until(c.lastRequest.Add(150 * time.Millisecond)); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	c.lastRequest = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json,text/html")
	response, err := c.client.Do(req)
	if err != nil {
		return nil, errors.New("public SEC request could not complete")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("public SEC response HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		return nil, errors.New("public SEC response is unreadable or exceeds its byte limit")
	}
	return raw, nil
}

func (c *collector) capture(ctx context.Context, symbol string, asOf time.Time) ([]record, error) {
	cik := issuers[symbol]
	endpoint := c.dataRoot + "/submissions/CIK" + cik + ".json"
	raw, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var submissions struct {
		CIK     json.RawMessage `json:"cik"`
		Name    string          `json:"name"`
		Tickers []string        `json:"tickers"`
		Filings struct {
			Recent recentFilings `json:"recent"`
		} `json:"filings"`
	}
	if err := json.Unmarshal(raw, &submissions); err != nil ||
		!matchesCIK(submissions.CIK, cik) || !slices.Contains(submissions.Tickers, symbol) {
		return nil, errors.New("SEC submissions do not verify the requested ticker and CIK")
	}
	filings, err := selectFilings(submissions.Filings.Recent, asOf)
	if err != nil {
		return nil, err
	}
	var earnings, periodic *filing
	for i := range filings {
		item := &filings[i]
		if earnings == nil && item.form == "8-K" && hasItem(item.items, "2.02") {
			earnings = item
		}
		if periodic == nil && (item.form == "10-Q" || item.form == "10-K") {
			periodic = item
		}
	}
	if earnings == nil && periodic == nil {
		return nil, errors.New("SEC recent filings contain no available earnings or periodic report")
	}
	var records []record
	if earnings != nil {
		payload := c.filingPayload(symbol, cik, *earnings, endpoint, raw)
		payload["headline"] = symbol + " filed an 8-K earnings release under Item 2.02"
		payload["summary"] = "The filing announces results; numerical quarterly statements, when present below, are separately bound to their own filing and period."
		c.attachRelease(ctx, payload, cik, *earnings)
		records = append(records, c.makeRecord("filing", "sec-earnings-"+earnings.accession, symbol, earnings.accepted, payload))
	}
	if periodic != nil {
		payload := c.filingPayload(symbol, cik, *periodic, endpoint, raw)
		payload["headline"] = symbol + " latest SEC " + periodic.form + " periodic statements"
		factsURL := c.dataRoot + "/api/xbrl/companyfacts/CIK" + cik + ".json"
		factsRaw, fetchErr := c.get(ctx, factsURL)
		if fetchErr == nil {
			facts, factErr := statementFacts(factsRaw, cik, *periodic)
			if factErr != nil {
				return nil, factErr
			}
			payload["statement_facts"] = facts
			payload["companyfacts_sha256"] = digest(factsRaw)
			payload["companyfacts_status"] = "captured"
		} else {
			payload["companyfacts_status"] = "unavailable"
			payload["companyfacts_error"] = fetchErr.Error()
		}
		payload["companyfacts_url"] = factsURL
		payload["statement_period_end"] = periodic.period
		if earnings != nil {
			payload["latest_earnings_accepted_at"] = earnings.accepted
			payload["summary"] = "These figures belong to the periodic filing and stated period. A newer earnings announcement does not update them."
		}
		records = append(records, c.makeRecord("filing", "sec-statements-"+periodic.accession, symbol, periodic.accepted, payload))
	}
	for _, item := range filings {
		if len(records) >= 7 {
			break
		}
		if item.form != "8-K" || item.accepted.Before(asOf.AddDate(0, 0, -90)) || !material(item.items) {
			continue
		}
		payload := c.filingPayload(symbol, cik, item, endpoint, raw)
		payload["headline"] = symbol + " SEC 8-K disclosure, Items " + item.items
		payload["summary"] = "Factual filing-item notice. The item codes alone do not establish a positive or negative investment impact."
		records = append(records, c.makeRecord("news", "sec-notice-"+item.accession, symbol, item.accepted, payload))
	}
	return records, nil
}

func selectFilings(recent recentFilings, asOf time.Time) ([]filing, error) {
	n := len(recent.Accessions)
	if n == 0 || len(recent.Accepted) != n || len(recent.Forms) != n || len(recent.Documents) != n || len(recent.Items) != n || len(recent.ReportDate) != n {
		return nil, errors.New("SEC recent filing columns differ in length or are empty")
	}
	var result []filing
	seen := map[string]bool{}
	for i, accession := range recent.Accessions {
		accepted, err := time.Parse(time.RFC3339Nano, recent.Accepted[i])
		if err != nil || !accessionPattern.MatchString(accession) || seen[accession] {
			return nil, errors.New("SEC filing accession or acceptance timestamp is invalid")
		}
		seen[accession] = true
		if accepted.After(asOf) {
			continue
		}
		if recent.Forms[i] != "8-K" && recent.Forms[i] != "10-Q" && recent.Forms[i] != "10-K" {
			continue
		}
		if !documentPattern.MatchString(recent.Documents[i]) {
			return nil, errors.New("SEC primary document path is unsupported")
		}
		result = append(result, filing{accession, recent.Forms[i], recent.Documents[i], recent.Items[i], recent.ReportDate[i], accepted.UTC()})
	}
	slices.SortFunc(result, func(a, b filing) int { return b.accepted.Compare(a.accepted) })
	return result, nil
}

func (c *collector) filingPayload(symbol, cik string, item filing, source string, raw []byte) map[string]any {
	return map[string]any{
		"symbol": symbol, "cik": cik, "accession_number": item.accession, "form": item.form, "items": item.items,
		"filing_report_date": item.period, "accepted_at": item.accepted, "source_url": c.documentURL(cik, item),
		"submissions_url": source, "submissions_sha256": digest(raw), "intraday_pit_verified": false,
	}
}

func (c *collector) documentURL(cik string, item filing) string {
	return c.archiveRoot + "/Archives/edgar/data/" + strings.TrimLeft(cik, "0") + "/" + strings.ReplaceAll(item.accession, "-", "") + "/" + item.document
}

func (c *collector) makeRecord(kind, id, symbol string, accepted time.Time, payload map[string]any) record {
	return record{kind, "sec-edgar", id, symbol, 1, accepted, accepted, c.now().UTC(), payload}
}

func (c *collector) attachRelease(ctx context.Context, payload map[string]any, cik string, item filing) {
	primary := c.documentURL(cik, item)
	raw, err := c.get(ctx, primary)
	if err != nil {
		payload["release_status"], payload["release_error"] = "unavailable", err.Error()
		return
	}
	payload["filing_document_sha256"] = digest(raw)
	base, _ := url.Parse(primary)
	var exhibitURL *url.URL
	for _, match := range hrefPattern.FindAllSubmatch(raw, -1) {
		candidate := resolveDocument(base, item.document, string(match[1]))
		if candidate != nil && (strings.Contains(strings.ToLower(candidate.Path), "ex99") ||
			strings.Contains(strings.ToLower(candidate.Path), "ex-99") || strings.Contains(strings.ToLower(candidate.Path), "exhibit99")) {
			exhibitURL = candidate
			break
		}
	}
	if exhibitURL == nil {
		indexURL := *base
		indexURL.Path = strings.TrimSuffix(base.Path, item.document) + item.accession + "-index.html"
		payload["filing_index_url"] = indexURL.String()
		index, err := c.get(ctx, indexURL.String())
		if err != nil {
			payload["release_status"], payload["release_error"] = "unavailable", err.Error()
			return
		}
		payload["filing_index_sha256"] = digest(index)
		for _, row := range tableRows.FindAll(index, -1) {
			if !exhibitType.Match(row) {
				continue
			}
			link := hrefPattern.FindSubmatch(row)
			if link != nil {
				exhibitURL = resolveDocument(base, item.document, string(link[1]))
			}
			if exhibitURL != nil {
				break
			}
		}
	}
	if exhibitURL == nil {
		payload["release_status"] = "no_exhibit_99_link_found"
		return
	}
	payload["exhibit_url"] = exhibitURL.String()
	exhibit, err := c.get(ctx, exhibitURL.String())
	if err != nil {
		payload["release_status"], payload["release_error"] = "unavailable", err.Error()
		return
	}
	payload["exhibit_sha256"] = digest(exhibit)
	text := []rune(strings.Join(strings.Fields(html.UnescapeString(markup.ReplaceAllString(activeHTML.ReplaceAllString(string(exhibit), " "), " "))), " "))
	payload["release_excerpt_truncated"] = len(text) > 16000
	if len(text) > 16000 {
		text = text[:16000]
	}
	payload["release_excerpt"], payload["release_status"] = string(text), "captured_exhibit"
}

func resolveDocument(base *url.URL, primary, href string) *url.URL {
	ref, err := url.Parse(html.UnescapeString(href))
	if err != nil {
		return nil
	}
	resolved := base.ResolveReference(ref)
	if resolved.Host != base.Host || resolved.Scheme != base.Scheme ||
		!strings.HasPrefix(resolved.Path, strings.TrimSuffix(base.Path, primary)) {
		return nil
	}
	return resolved
}

func hasItem(items, wanted string) bool {
	for _, item := range strings.Split(items, ",") {
		if strings.TrimSpace(item) == wanted {
			return true
		}
	}
	return false
}

func material(items string) bool {
	for _, item := range []string{"1.01", "1.02", "2.01", "2.02", "2.03", "2.04", "2.05", "2.06", "3.01", "3.02", "5.02", "5.03", "5.07", "7.01", "8.01"} {
		if hasItem(items, item) {
			return true
		}
	}
	return false
}

type statementValue struct {
	Concept string `json:"concept"`
	Unit    string `json:"unit"`
	Start   string `json:"period_start,omitempty"`
	End     string `json:"period_end"`
	Value   string `json:"value"`
}

func statementFacts(raw []byte, cik string, report filing) ([]statementValue, error) {
	var document struct {
		CIK   json.RawMessage `json:"cik"`
		Facts map[string]map[string]struct {
			Units map[string][]struct {
				Accession string      `json:"accn"`
				Form      string      `json:"form"`
				Filed     string      `json:"filed"`
				Start     string      `json:"start"`
				End       string      `json:"end"`
				Value     json.Number `json:"val"`
			} `json:"units"`
		} `json:"facts"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil || !matchesCIK(document.CIK, cik) {
		return nil, errors.New("SEC companyfacts do not identify the expected filer")
	}
	result := []statementValue{}
	seen := map[string]string{}
	concepts := []string{"Revenues", "RevenueFromContractWithCustomerExcludingAssessedTax", "RevenueFromContractWithCustomerIncludingAssessedTax",
		"NetIncomeLoss", "GrossProfit", "OperatingIncomeLoss", "Assets", "Liabilities", "CashAndCashEquivalentsAtCarryingValue",
		"NetCashProvidedByUsedInOperatingActivities", "PaymentsToAcquirePropertyPlantAndEquipment"}
	for _, concept := range concepts {
		for _, fact := range document.Facts["us-gaap"][concept].Units["USD"] {
			if fact.Accession != report.accession || fact.Form != report.form || fact.End != report.period {
				continue
			}
			filed, err := time.Parse("2006-01-02", fact.Filed)
			if err != nil || filed.After(report.accepted) {
				return nil, errors.New("SEC fact filing date is after its accepted submission")
			}
			amount, ok := new(big.Rat).SetString(fact.Value.String())
			if !ok {
				return nil, errors.New("SEC fact is not an exact number")
			}
			value := strings.TrimRight(strings.TrimRight(amount.FloatString(8), "0"), ".")
			check, _ := new(big.Rat).SetString(value)
			if check == nil || check.Cmp(amount) != 0 {
				return nil, errors.New("SEC fact exceeds supported exact decimal precision")
			}
			key := concept + "\n" + fact.Start + "\n" + fact.End
			if prior, exists := seen[key]; exists {
				if prior != value {
					return nil, errors.New("SEC fact identity has conflicting numeric values")
				}
				continue
			}
			seen[key] = value
			result = append(result, statementValue{concept, "USD", fact.Start, fact.End, value})
		}
	}
	slices.SortFunc(result, func(a, b statementValue) int { return strings.Compare(a.Concept+"\n"+a.Start, b.Concept+"\n"+b.Start) })
	return result, nil
}

func digest(raw []byte) string {
	value := sha256.Sum256(raw)
	return hex.EncodeToString(value[:])
}

func matchesCIK(raw json.RawMessage, expected string) bool {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return false
		}
		value = number.String()
	}
	return strings.TrimLeft(value, "0") == strings.TrimLeft(expected, "0")
}
