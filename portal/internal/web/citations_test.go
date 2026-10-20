package web

import (
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
)

func TestCitationMarkdownPresentation(t *testing.T) {
	s := newTestServer(t)
	token := "\ue200cite\ue202ref\ue201"
	prefix := "Příliš 🐈 **answer** "
	entry := codex.TranscriptEntry{Kind: "agentMessage", TurnStatus: "completed", Text: prefix + token + "\n\n`" + token + "`\n\n<script>alert(1)</script>", Citations: []codex.CitationSpan{{Start: len(prefix), End: len(prefix) + len(token), Sources: []codex.CitationSource{{URL: "https://pypi.org/help/", Title: `Help · PyPI "quoted" <script>alert(1)</script>`, Number: 1}}}}}
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{entry}}
	s.presentTranscript(&transcript)
	got := transcript.Entries[0]
	if got.Text != entry.Text || !strings.Contains(got.HTML, `href="https://pypi.org/help/"`) || !strings.Contains(got.HTML, "[1]</a>") || !strings.Contains(got.HTML, "<strong>answer</strong>") {
		t.Fatalf("presentation = %#v", got)
	}
	if !strings.Contains(got.HTML, `title="Help · PyPI &#34;quoted&#34; &lt;script&gt;alert(1)&lt;/script&gt;"`) {
		t.Fatalf("source title = %s", got.HTML)
	}
	if strings.Contains(got.HTML, "<script>") || strings.Count(got.HTML, token) != 1 {
		t.Fatalf("sanitization/code preservation = %s", got.HTML)
	}
	transcript.Entries[0].Citations[0].Sources[0].URL = "javascript:alert(1)"
	s.presentTranscript(&transcript)
	if strings.Contains(transcript.Entries[0].HTML, "javascript:") {
		t.Fatal("unsafe source URL rendered")
	}
}
