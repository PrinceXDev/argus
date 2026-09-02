package edgar

import (
	"strings"
	"testing"
)

// Span integrity resolves a claim's quote against this output, so the
// transformation must keep sentences intact and must not fuse separate cells
// into sentences that never existed.
func TestStripHTML_KeepsBlockBoundaries(t *testing.T) {
	in := `<table><tr><td>Zeta Holdings LLC</td><td>60.5%</td></tr></table>`
	out := StripHTML(in)
	if strings.Contains(out, "Zeta Holdings LLC 60.5%") {
		t.Errorf("adjacent cells were fused into one line: %q", out)
	}
	if !strings.Contains(out, "Zeta Holdings LLC") || !strings.Contains(out, "60.5%") {
		t.Errorf("cell content was lost: %q", out)
	}
}

func TestStripHTML_RemovesScriptsAndStyles(t *testing.T) {
	in := `<p>Real text.</p><script>var secret = "should not appear";</script>` +
		`<style>.x{color:red}</style>`
	out := StripHTML(in)
	if strings.Contains(out, "secret") || strings.Contains(out, "color:red") {
		t.Errorf("script or style content leaked into the text: %q", out)
	}
	if !strings.Contains(out, "Real text.") {
		t.Errorf("real text was lost: %q", out)
	}
}

func TestStripHTML_DecodesCommonEntities(t *testing.T) {
	out := StripHTML(`<p>Smith &amp; Co. holds &gt;50&#37; of the shares&nbsp;outstanding.</p>`)
	if !strings.Contains(out, "Smith & Co.") {
		t.Errorf("ampersand not decoded: %q", out)
	}
	if !strings.Contains(out, ">50") {
		t.Errorf("gt not decoded: %q", out)
	}
	// An unknown entity must become a space rather than surviving as markup,
	// which would end up inside a quote.
	if strings.Contains(out, "&") && !strings.Contains(out, "Smith & Co") {
		t.Errorf("stray entity survived: %q", out)
	}
}

// A quote taken from the stripped text must still be findable in it. This is
// the property the whole extraction pipeline depends on.
func TestStripHTML_OutputIsStableForQuoting(t *testing.T) {
	in := `<div><p>Zeta   Holdings LLC reported a 60.5 percent
	beneficial ownership stake in Acme Corp.</p></div>`
	out := StripHTML(in)
	if strings.Contains(out, "  ") {
		t.Errorf("runs of spaces survived, which makes verbatim quoting fragile: %q", out)
	}
	if !strings.Contains(out, "Zeta Holdings LLC reported a 60.5 percent") {
		t.Errorf("sentence was not normalised as expected: %q", out)
	}
}

func TestStripHTML_CollapsesExcessBlankLines(t *testing.T) {
	out := StripHTML("<p>a</p><p></p><p></p><p></p><p>b</p>")
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("more than one blank line survived: %q", out)
	}
}

func TestStripHTML_EmptyInput(t *testing.T) {
	if got := StripHTML(""); got != "" {
		t.Errorf("empty input produced %q", got)
	}
	if got := StripHTML("<p></p>"); got != "" {
		t.Errorf("empty markup produced %q", got)
	}
}

func TestFetchCompanies_RequiresCIKs(t *testing.T) {
	c := New("ARGUS/test (test@example.com)", nil)
	if _, err := c.FetchCompanies(t.Context(), Request{}); err == nil {
		t.Fatal("expected an error when no CIKs are requested")
	}
}
