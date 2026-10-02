package creationprogress

import (
	"bytes"
	"strings"
	"testing"
)

func TestDecoderStreamsSplitFramesAndDiagnostics(t *testing.T) {
	input := "before\n" + Prefix + `{"stage":"team_member","event":"begin","elapsedMs":0,"member":"architect0"}` + "\n" +
		"between" + Prefix + `{"stage":"team_member","event":"finish","elapsedMs":12,"member":"architect0"}` + "\nafter"
	for _, size := range []int{1, 3, len(input)} {
		var diagnostics bytes.Buffer
		var events []Event
		decoder := NewDecoder(&diagnostics, func(event Event) { events = append(events, event) })
		for offset := 0; offset < len(input); offset += size {
			end := min(offset+size, len(input))
			if _, err := decoder.Write([]byte(input[offset:end])); err != nil {
				t.Fatal(err)
			}
		}
		if err := decoder.Close(); err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].Event != "begin" || events[1].ElapsedMs != 12 || events[1].Member != "architect0" {
			t.Fatalf("events = %#v", events)
		}
		if diagnostics.String() != "before\nbetweenafter" {
			t.Fatalf("diagnostics = %q", diagnostics.String())
		}
	}
}

func TestDecoderRejectsMalformedProgressAndContinues(t *testing.T) {
	for _, record := range []string{
		`{"stage":"prompt","event":"begin","elapsedMs":0,"state":"ready"}`,
		`{"stage":"prompt","stage":"terminal","event":"begin","elapsedMs":0}`,
		`{"stage":"ready","event":"finish","elapsedMs":1}`,
		`{"stage":"prompt","event":"begin","elapsedMs":1}`,
		`{"stage":"prompt","event":"finish","elapsedMs":-1}`,
		`{"stage":"prompt","event":"finish","elapsedMs":86400001}`,
		`{"stage":"prompt","event":"finish","elapsedMs":1.5}`,
		`{"stage":"prompt","event":"finish","elapsedMs":null}`,
		`{"stage":"team_member","event":"finish","elapsedMs":1,"member":null}`,
		`{"stage":"team_member","event":"finish","elapsedMs":1,"member":"../other"}`,
		`{`,
	} {
		var diagnostics bytes.Buffer
		events := 0
		decoder := NewDecoder(&diagnostics, func(Event) { events++ })
		bad := Prefix + record + "\n"
		_, _ = decoder.Write([]byte(bad + Prefix + `{"stage":"prompt","event":"finish","elapsedMs":2}` + "\n"))
		_ = decoder.Close()
		if events != 1 || diagnostics.String() != bad {
			t.Fatalf("record %q: events=%d diagnostics=%q", record, events, diagnostics.String())
		}
	}
}

func TestDecoderBoundsOversizedAndTruncatedRecords(t *testing.T) {
	var diagnostics bytes.Buffer
	events := 0
	decoder := NewDecoder(&diagnostics, func(Event) { events++ })
	_, _ = decoder.Write([]byte(Prefix + strings.Repeat("x", 100000) + "\n" +
		Prefix + `{"stage":"evidence","event":"finish","elapsedMs":3}` + "\n" + Prefix + `{`))
	_ = decoder.Close()
	if events != 1 || diagnostics.Len() > 200 || !strings.Contains(diagnostics.String(), "omitted") || !strings.HasSuffix(diagnostics.String(), Prefix+"{") {
		t.Fatalf("events=%d diagnostics=%q", events, diagnostics.String())
	}
	decoder = NewDecoder(&diagnostics, nil)
	for index := 0; index < 10000; index++ {
		_, _ = decoder.Write([]byte(Prefix + "bad\n"))
	}
	if diagnostics.Len() > 17000 {
		t.Fatalf("rejected diagnostics grew to %d", diagnostics.Len())
	}
}

func TestBeginEmitsSuccessOnlyStageDuration(t *testing.T) {
	var events []Event
	finish := Begin(func(event Event) { events = append(events, event) }, "conversation", "")
	if len(events) != 1 || !events[0].Valid() || events[0].ElapsedMs != 0 {
		t.Fatalf("begin = %#v", events)
	}
	finish()
	if len(events) != 2 || !events[1].Valid() || events[1].Event != "finish" {
		t.Fatalf("finish = %#v", events)
	}
	Begin(nil, "conversation", "")()
}
