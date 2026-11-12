package repository

import "testing"

func TestReviewCountsRejectIncompleteAndMismatchedRecords(t *testing.T) {
	files := []ReviewFile{{Path: "new\tname\n", OldPath: "old\n", CountsPending: true}}
	good := "2\t3\t\x00old\n\x00new\tname\n\x00"
	counted, err := parseReviewCounts([]byte(good), files)
	if err != nil || *counted[0].Additions != 2 || *counted[0].Deletions != 3 || counted[0].CountsPending || !files[0].CountsPending {
		t.Fatal(counted, err)
	}
	for _, input := range []string{"", good + good, "2\t3\tother\x00", "2\t3\t\x00old\n\x00", "-1\t3\t\x00old\n\x00new\tname\n\x00", good[:len(good)-1]} {
		if _, err := parseReviewCounts([]byte(input), files); err == nil {
			t.Fatalf("accepted incomplete or invalid counts %q", input)
		}
	}
}
