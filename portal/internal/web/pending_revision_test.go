package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/codex-web/conversation"
)

type revisionRecoveryCodex struct{ *recoveryTestCodex }

func (c revisionRecoveryCodex) PendingRevision(thread string) uint64 {
	if thread == "thread-1" {
		return 17
	}
	return 0
}

type pagedRevisionRecoveryCodex struct{ revisionRecoveryCodex }

func (c pagedRevisionRecoveryCodex) ReadThreadPage(context.Context, string, string) (codex.TranscriptPage, error) {
	return codex.TranscriptPage{}, nil
}

func TestPendingRevisionForwarding(t *testing.T) {
	for _, kind := range []string{"legacy", "revision", "paged-revision"} {
		t.Run(kind, func(t *testing.T) {
			s, base := recoveryTestServer(t)
			defer s.Close()
			manifestPath := filepath.Join(s.config.Workspace, "work", "example", "portal.yml")
			manifest, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, []byte(strings.ReplaceAll(string(manifest), "0.160.0", "0.152.1")), 0644); err != nil {
				t.Fatal(err)
			}
			writeWebRuntimeAuthority(t, s, "example")
			if kind == "revision" {
				s.config.Codex = revisionRecoveryCodex{base}
			} else if kind == "paged-revision" {
				s.config.Codex = pagedRevisionRecoveryCodex{revisionRecoveryCodex{base}}
			}
			target, err := s.resolveConversation(context.Background(), conversation.ResolveRequest{ID: "example", Operation: "thread"})
			if err != nil {
				t.Fatal(err)
			}
			provider, ok := target.Client.(pendingRevisionProvider)
			if ok != (kind != "legacy") {
				t.Fatalf("optional pending capability = %t", ok)
			}
			if ok && (provider.PendingRevision("thread-1") != 17 || provider.PendingRevision("other") != 0) {
				t.Fatal("revision changed while forwarding")
			}
			_, paged := target.Client.(conversation.TranscriptPageReader)
			if paged != (kind == "paged-revision") {
				t.Fatal("paging capability changed")
			}
			if _, ok := target.Client.(conversation.QueueDeletionCompleter); !ok {
				t.Fatal("queue deletion recovery was lost")
			}
		})
	}
}
