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

func TestCitationsComposeWithDecoratedUserMarkdownAndTeamPresentation(t *testing.T) {
	s := newTestServer(t)
	token := "\ue200cite\ue202ref\ue201"
	prefix := "Příliš 🐈 **answer** "
	canonical := prefix + token + "\n\n`" + token + "`"
	display := "# User heading\n\n" + token
	empty := ""
	agent := codex.TranscriptEntry{Kind: "agentMessage", TurnStatus: "completed", Text: canonical, ClientUserMessageDigest: "unchanged-digest", CitationsPending: true,
		Citations: []codex.CitationSpan{{Start: len(prefix), End: len(prefix) + len(token)}}}
	user := codex.TranscriptEntry{Kind: "userMessage", Text: "attachment envelope", DisplayText: &display, Citations: agent.Citations, Attachments: []codex.Attachment{{ID: "file", Name: "file.txt"}}}
	transcript := codex.Transcript{Entries: []codex.TranscriptEntry{agent, user, {Kind: "userMessage", Text: "Team assignment from lead to implementer0:\n\nWork."}, {Kind: "userMessage", Text: "hidden envelope", DisplayText: &empty}}}
	s.presentTranscript(&transcript)
	if !strings.Contains(transcript.Entries[0].HTML, "[source pending]") {
		t.Fatal("pending citation disappeared")
	}
	if !strings.Contains(transcript.Entries[1].HTML, "<h1>User heading</h1>") || !strings.Contains(transcript.Entries[1].HTML, token) || strings.Contains(transcript.Entries[1].HTML, "attachment envelope") {
		t.Fatalf("user citation/display handling: %s", transcript.Entries[1].HTML)
	}
	if transcript.Entries[2].Communication == nil || transcript.Entries[2].Communication.To != "implementer0" || transcript.Entries[3].HTML != "" {
		t.Fatal("team/empty user display changed")
	}
	// A late source update must render from original UTF-8 offsets even after the
	// message is retained outside the newest page. Neither canonical text changes.
	transcript.Entries[0].CitationsPending = false
	transcript.Entries[0].Citations[0].Sources = []codex.CitationSource{{URL: "https://example.test/source", Title: `Český "title" <script>`, Number: 1}}
	s.presentTranscript(&transcript)
	got := transcript.Entries[0]
	if got.Text != canonical || got.ClientUserMessageDigest != "unchanged-digest" || !strings.Contains(got.HTML, `href="https://example.test/source"`) || !strings.Contains(got.HTML, `title="Český &#34;title&#34; &lt;script&gt;"`) || strings.Contains(got.HTML, "<script>") || strings.Count(got.HTML, token) != 1 {
		t.Fatalf("citation composition: %#v", got)
	}
	if transcript.Entries[1].Text != "attachment envelope" || *transcript.Entries[1].DisplayText != display || len(transcript.Entries[1].Attachments) != 1 {
		t.Fatal("user canonical/attachments changed")
	}
}
