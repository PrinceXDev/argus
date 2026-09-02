// Package edgar fetches filings from the SEC's EDGAR system.
//
// EDGAR is used because it is the only large, real, structurally rich corpus
// that is unambiguously safe for this project: US federal government works are
// public domain, the bulk endpoints are free and documented, and the filings
// are exactly the ownership and control documents the investigation domain is
// about.
//
// Two SEC requirements are honoured, and both are hard rules rather than
// courtesies. Every request carries a descriptive User-Agent with a contact
// address, and requests are rate-limited to 10 per second. The SEC blocks
// clients that ignore either.
package edgar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/prince/argus/internal/ingest"
)

const (
	submissionsURL = "https://data.sec.gov/submissions/CIK%010s.json"
	archiveURL     = "https://www.sec.gov/Archives/edgar/data/%s/%s/%s"

	// rateInterval keeps requests at or under the SEC's documented 10/second
	// ceiling. Exceeding it gets the client blocked, so the limiter is
	// unconditional rather than configurable.
	rateInterval = 110 * time.Millisecond
)

// Client fetches EDGAR filings.
type Client struct {
	userAgent string
	http      *http.Client
	log       *slog.Logger
	limiter   <-chan time.Time
}

// New builds a client. The user agent must identify the application and carry a
// contact email, per the SEC's access policy.
func New(userAgent string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		userAgent: userAgent,
		http:      &http.Client{Timeout: 30 * time.Second},
		log:       log,
		limiter:   time.Tick(rateInterval),
	}
}

// Request describes what to fetch.
type Request struct {
	// CIKs are SEC Central Index Keys, with or without leading zeros.
	CIKs []string
	// Forms filters by form type ("SC 13D", "4", "8-K"). Empty means all.
	Forms []string
	// Limit caps filings per company.
	Limit int
}

// FetchCompanies retrieves filings for each CIK and returns them ready to
// ingest.
//
// A failure on one company is logged and skipped rather than aborting the run:
// EDGAR intermittently rate-limits or 404s individual documents, and losing an
// entire ingest to one bad filing would make the corpus unbuildable.
func (c *Client) FetchCompanies(ctx context.Context, req Request) ([]ingest.RawDocument, error) {
	if len(req.CIKs) == 0 {
		return nil, fmt.Errorf("edgar: no CIKs requested")
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	var out []ingest.RawDocument
	for _, cik := range req.CIKs {
		docs, err := c.fetchCompany(ctx, cik, req)
		if err != nil {
			c.log.Warn("edgar: skipping company", "cik", cik, "err", err)
			continue
		}
		out = append(out, docs...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("edgar: no filings retrieved for any of %d CIK(s)", len(req.CIKs))
	}
	return out, nil
}

// submissions is the subset of the SEC's submissions JSON that we read.
type submissions struct {
	CIK     string `json:"cik"`
	Name    string `json:"name"`
	Filings struct {
		Recent struct {
			AccessionNumber []string `json:"accessionNumber"`
			FilingDate      []string `json:"filingDate"`
			Form            []string `json:"form"`
			PrimaryDocument []string `json:"primaryDocument"`
			ReportDate      []string `json:"reportDate"`
		} `json:"recent"`
	} `json:"filings"`
}

func (c *Client) fetchCompany(ctx context.Context, cik string, req Request) ([]ingest.RawDocument, error) {
	clean := strings.TrimLeft(strings.TrimSpace(cik), "0")
	if clean == "" {
		return nil, fmt.Errorf("edgar: empty CIK")
	}

	body, err := c.get(ctx, fmt.Sprintf(submissionsURL, clean))
	if err != nil {
		return nil, err
	}
	var subs submissions
	if err := json.Unmarshal(body, &subs); err != nil {
		return nil, fmt.Errorf("edgar: decoding submissions for CIK %s: %w", clean, err)
	}

	want := map[string]bool{}
	for _, f := range req.Forms {
		want[strings.ToUpper(strings.TrimSpace(f))] = true
	}

	r := subs.Filings.Recent
	// The submissions payload is column-oriented: parallel arrays that must be
	// the same length. A ragged payload means the API shape changed, and reading
	// it positionally anyway would silently mismatch dates to documents.
	n := len(r.AccessionNumber)
	if len(r.Form) != n || len(r.FilingDate) != n || len(r.PrimaryDocument) != n {
		return nil, fmt.Errorf("edgar: submissions arrays are ragged for CIK %s "+
			"(accessions=%d forms=%d dates=%d docs=%d)",
			clean, n, len(r.Form), len(r.FilingDate), len(r.PrimaryDocument))
	}

	var out []ingest.RawDocument
	for i := 0; i < n && len(out) < req.Limit; i++ {
		if len(want) > 0 && !want[strings.ToUpper(r.Form[i])] {
			continue
		}
		if r.PrimaryDocument[i] == "" {
			continue
		}
		accession := strings.ReplaceAll(r.AccessionNumber[i], "-", "")
		url := fmt.Sprintf(archiveURL, clean, accession, r.PrimaryDocument[i])

		raw, err := c.get(ctx, url)
		if err != nil {
			c.log.Warn("edgar: skipping filing", "url", url, "err", err)
			continue
		}
		text := StripHTML(string(raw))
		if len(strings.TrimSpace(text)) < 200 {
			// Exhibit stubs and cover pages carry no extractable assertion.
			continue
		}

		filed, _ := time.Parse("2006-01-02", r.FilingDate[i])
		sum := sha256.Sum256(raw)
		out = append(out, ingest.RawDocument{
			URL:         url,
			Title:       fmt.Sprintf("%s - %s (%s)", r.Form[i], subs.Name, r.FilingDate[i]),
			Text:        text,
			PublishedAt: filed,
			SHA256:      hex.EncodeToString(sum[:]),
			Source: ingest.Source{
				Name: "SEC EDGAR",
				Kind: "filing",
				// A regulatory filing carries legal consequence for
				// misstatement, which is a real reason to weight it above a news
				// report. It is still not 1.0: filings are amended and restated.
				Trust: 0.95,
			},
		})
	}
	return out, nil
}

// get performs a rate-limited, identified GET.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	select {
	case <-c.limiter:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Encoding", "gzip, deflate")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("edgar: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("edgar: 403 for %s - the SEC rejects requests without a "+
			"descriptive User-Agent containing a contact email (check EDGAR_USER_AGENT)", url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("edgar: GET %s: HTTP %d", url, resp.StatusCode)
	}
	// Filings can be large; the cap prevents one pathological exhibit from
	// exhausting memory.
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

var (
	scriptStyle = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	tagRe       = regexp.MustCompile(`(?s)<[^>]+>`)
	entityRe    = regexp.MustCompile(`&[a-zA-Z#0-9]+;`)
	wsRe        = regexp.MustCompile(`[ \t]+`)
	blankRe     = regexp.MustCompile(`\n{3,}`)
)

var entities = map[string]string{
	"&nbsp;": " ", "&amp;": "&", "&lt;": "<", "&gt;": ">",
	"&quot;": `"`, "&apos;": "'", "&#39;": "'", "&#160;": " ",
	"&rsquo;": "'", "&lsquo;": "'", "&ldquo;": `"`, "&rdquo;": `"`,
	"&mdash;": "-", "&ndash;": "-",
}

// StripHTML reduces an EDGAR filing to plain text while preserving paragraph
// structure.
//
// Structure matters more here than in a typical scraper. Span integrity
// resolves a claim's quote against this text, so whatever transformation
// happens must be stable and must not mangle the sentences a quote will be
// taken from. Block-level tags become newlines rather than disappearing, so
// table cells and paragraphs do not run together into sentences that never
// existed.
func StripHTML(s string) string {
	s = scriptStyle.ReplaceAllString(s, " ")

	// Block-level boundaries become newlines before tags are stripped, so
	// adjacent cells and paragraphs stay separate.
	for _, tag := range []string{"</p>", "</div>", "</tr>", "<br>", "<br/>", "<br />",
		"</h1>", "</h2>", "</h3>", "</td>", "</table>", "</li>"} {
		s = strings.ReplaceAll(s, tag, tag+"\n")
		s = strings.ReplaceAll(s, strings.ToUpper(tag), tag+"\n")
	}

	s = tagRe.ReplaceAllString(s, " ")

	s = entityRe.ReplaceAllStringFunc(s, func(e string) string {
		if v, ok := entities[strings.ToLower(e)]; ok {
			return v
		}
		return " "
	})

	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = wsRe.ReplaceAllString(s, " ")

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
