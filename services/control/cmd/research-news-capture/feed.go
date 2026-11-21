package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	maxFeedBytes = 2 * 1024 * 1024
	atomSpace    = "http://www.w3.org/2005/Atom"
)

var (
	// Feed summaries can embed HTML as escaped text or CDATA. Drop markup and
	// active blocks without fetching their resources or rendering the page.
	activeMarkup = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>`)
	tagMarkup    = regexp.MustCompile(`(?s)</?[A-Za-z][^>]*>|<!--.*?-->`)
	nvdaEarnings = regexp.MustCompile(`(?i)^NVIDIA Announces Financial Results for (?:First|Second|Third|Fourth) Quarter Fiscal [0-9]{4}$`)
)

type feedDocument struct {
	XMLName xml.Name
	RSS     []rssItem   `xml:"channel>item"`
	Atom    []atomEntry `xml:"entry"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Publication string `xml:"pubDate"`
	Updated     string `xml:"modDate"`
	Description string `xml:"description"`
	Content     string `xml:"content"`
}

type atomEntry struct {
	Title     string     `xml:"title"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Links     []atomLink `xml:"link"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type feedArticle struct {
	title, link, timestamp, timestampKind string
	updated, summary, content             string
}

func collectSource(ctx context.Context, client *http.Client, source issuerSource, limit int, maxAge time.Duration, now func() time.Time) ([]archiveRecord, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/rss+xml, application/atom+xml, text/xml, application/xml")
	request.Header.Set("User-Agent", "TradingBot 0.1 research reader")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	receivedAt := now().UTC().Truncate(time.Microsecond)
	if len(raw) > maxFeedBytes {
		return nil, errors.New("feed exceeds response size limit")
	}
	articles, err := decodeFeed(raw)
	if err != nil {
		return nil, err
	}
	from := receivedAt.Add(-maxAge)
	records := make([]archiveRecord, 0, len(articles))
	for _, article := range articles {
		if record, ok := buildRecord(source, article, receivedAt, from); ok {
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].EventTime.Equal(records[j].EventTime) {
			return records[i].EventTime.After(records[j].EventTime)
		}
		if !records[i].AvailableAt.Equal(records[j].AvailableAt) {
			return records[i].AvailableAt.After(records[j].AvailableAt)
		}
		return records[i].RecordID < records[j].RecordID
	})
	selected := make([]archiveRecord, 0, min(limit, len(records)))
	seen := make(map[string]bool)
	for _, record := range records {
		if seen[record.RecordID] {
			continue
		}
		seen[record.RecordID] = true
		selected = append(selected, record)
		if len(selected) == limit {
			break
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("no dated, recent entries on official issuer URLs")
	}
	return selected, nil
}

func decodeFeed(raw []byte) ([]feedArticle, error) {
	var feed feedDocument
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&feed); err != nil {
		return nil, fmt.Errorf("decode issuer feed: %w", err)
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode issuer feed: %w", err)
		}
		if ch, ok := token.(xml.CharData); !ok || len(bytes.TrimSpace(ch)) != 0 {
			return nil, errors.New("unexpected XML after issuer feed")
		}
	}
	var articles []feedArticle
	switch feed.XMLName {
	case xml.Name{Local: "rss"}:
		for _, row := range feed.RSS {
			articles = append(articles, feedArticle{
				title: row.Title, link: row.Link, timestamp: row.Publication, timestampKind: "rss_pub_date",
				updated: row.Updated, summary: row.Description, content: row.Content,
			})
		}
	case xml.Name{Space: atomSpace, Local: "feed"}:
		for _, row := range feed.Atom {
			stamp, stampKind, updated := row.Published, "atom_published", row.Updated
			if strings.TrimSpace(stamp) == "" {
				stamp, stampKind, updated = updated, "atom_updated", ""
			}
			summary := row.Summary
			if strings.TrimSpace(summary) == "" {
				summary = row.Content
			}
			for _, link := range row.Links {
				if link.Rel == "" || link.Rel == "alternate" {
					articles = append(articles, feedArticle{
						title: row.Title, link: link.Href, timestamp: stamp, timestampKind: stampKind,
						updated: updated, summary: summary, content: row.Content,
					})
					break
				}
			}
		}
	default:
		return nil, errors.New("expected an RSS or Atom issuer feed")
	}
	return articles, nil
}

func buildRecord(source issuerSource, article feedArticle, receivedAt, from time.Time) (archiveRecord, bool) {
	link := strings.TrimSpace(article.link)
	canonical, ok := issuerLink(link, source.Domains)
	title := compactText(article.title, 300)
	stamp := strings.TrimSpace(article.timestamp)
	at := observationTime(stamp)
	if !ok || title == "" || at.IsZero() || at.Before(from) || at.After(receivedAt) {
		return archiveRecord{}, false
	}
	availability := at
	updated := strings.TrimSpace(article.updated)
	if updated != "" {
		changedAt := observationTime(updated)
		if changedAt.IsZero() || changedAt.After(receivedAt) {
			return archiveRecord{}, false
		}
		if changedAt.After(availability) {
			availability = changedAt
		}
	}
	kind, evidenceKind := "news", "issuer_announcement"
	summary := compactText(article.summary, 640)
	if source.Provider == "nvidia-newsroom" && nvdaEarnings.MatchString(title) {
		release := compactText(article.content, 3200)
		if strings.Contains(strings.ToLower(release), "revenue") {
			kind, evidenceKind, summary = "filing", "issuer_earnings_release", release
		}
	}
	sum := sha256.Sum256([]byte(canonical))
	return archiveRecord{
		Kind: kind, Provider: source.Provider, RecordID: hex.EncodeToString(sum[:]), Symbol: source.Symbol, Revision: 1,
		EventTime: at, AvailableAt: availability, IngestedAt: receivedAt,
		Payload: newsEvidence{
			Headline: title, Summary: summary, URL: link, FeedURL: source.URL, Timestamp: stamp,
			TimestampKind: article.timestampKind, Updated: updated, EvidenceKind: evidenceKind,
		},
	}, true
}

func issuerLink(link string, domains []string) (string, bool) {
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			parsed.Host = host
			parsed.Fragment = ""
			return parsed.String(), true
		}
	}
	return "", false
}

func observationTime(raw string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC822Z} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at.UTC().Truncate(time.Microsecond)
		}
	}
	if strings.HasSuffix(raw, " GMT") || strings.HasSuffix(raw, " UTC") {
		if at, err := time.Parse(time.RFC1123, raw); err == nil {
			return at.UTC()
		}
	}
	return time.Time{}
}

func compactText(text string, limit int) string {
	text = html.UnescapeString(text)
	text = activeMarkup.ReplaceAllString(text, " ")
	text = tagMarkup.ReplaceAllString(text, " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if chars := []rune(text); len(chars) > limit {
		text = strings.TrimSpace(string(chars[:limit]))
	}
	return text
}
