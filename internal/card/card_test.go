package card_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/card"
)

const (
	testUUID = "0b8f8e3c-1d2a-4e5f-9a6b-7c8d9e0f1a2b"
	testFp   = "ba7816bf8f01"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	t.Parallel()
	var actions []card.Action
	for _, v := range []string{"resolve", "rerun", "keep", "unlock", "preview", "refresh", "cancel"} {
		actions = append(actions, card.Action{Kind: "inc", Verb: v, Args: []string{testUUID, "awaiting_owner"}})
	}
	for _, v := range []string{"approve", "confirm"} {
		actions = append(actions, card.Action{Kind: "inc", Verb: v, Args: []string{testUUID, "awaiting_owner", testFp}})
	}
	for _, v := range []string{"import", "regrab", "remove"} {
		actions = append(actions, card.Action{Kind: "sq", Verb: v, Args: []string{"s", "SABnzbd_nzo_x1", testFp}})
	}
	for _, a := range actions {
		id, ok := card.Encode(a)
		if !ok {
			t.Fatalf("Encode(%+v) not ok", a)
		}
		got, err := card.Decode(id)
		if err != nil {
			t.Fatalf("Decode(%q): %v", id, err)
		}
		if got.Kind != a.Kind || got.Verb != a.Verb || !slices.Equal(got.Args, a.Args) {
			t.Errorf("round trip %q = %+v, want %+v", id, got, a)
		}
	}
}

func TestEncode_LongestIDFits(t *testing.T) {
	t.Parallel()
	id, ok := card.Encode(card.Action{
		Kind: "inc", Verb: "confirm", Args: []string{testUUID, "manual_test_needed", testFp},
	})
	if !ok || len(id) != 84 {
		t.Errorf("Encode = %q (len %d), %v; want len 84, true", id, len(id), ok)
	}
}

func TestEncode_RejectsLongOrColon(t *testing.T) {
	t.Parallel()
	long := card.Action{
		Kind: "sq",
		Verb: "remove",
		Args: []string{strings.Repeat("x", card.MaxIDLen-len("mf1:sq:remove:")+1)},
	}
	if id, ok := card.Encode(long); ok {
		t.Errorf("Encode(101 chars) = %q, true; want false", id)
	}
	fits := card.Action{
		Kind: "sq",
		Verb: "remove",
		Args: []string{strings.Repeat("x", card.MaxIDLen-len("mf1:sq:remove:"))},
	}
	if _, ok := card.Encode(fits); !ok {
		t.Error("Encode(100 chars) = false; want true")
	}
	if id, ok := card.Encode(card.Action{Kind: "sq", Verb: "remove", Args: []string{"a:b"}}); ok {
		t.Errorf("Encode(arg with colon) = %q, true; want false", id)
	}
}

func TestDecode_UnknownPrefix(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"mf0:inc:x", "mf1:inc", "report", ""} {
		if _, err := card.Decode(id); !errors.Is(err, card.ErrUnknownID) {
			t.Errorf("Decode(%q) err = %v, want ErrUnknownID", id, err)
		}
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	if got := card.Fingerprint([]byte("abc")); got != testFp {
		t.Errorf("Fingerprint(abc) = %q, want %q", got, testFp)
	}
}

func TestSubjectOf(t *testing.T) {
	t.Parallel()
	sq := card.Action{Kind: "sq", Verb: "remove", Args: []string{"s", "ABC_def", testFp}}
	if got := card.SubjectOf(sq); got != "sq:s:abc_def" || got != card.StuckSubject("s", "ABC_def") {
		t.Errorf("SubjectOf(sq) = %q, want sq:s:abc_def", got)
	}
	inc := card.Action{Kind: "inc", Verb: "resolve", Args: []string{testUUID, "open"}}
	if got := card.SubjectOf(inc); got != "inc:"+testUUID || got != card.IncidentSubject(testUUID) {
		t.Errorf("SubjectOf(inc) = %q", got)
	}
	if got := card.SubjectOf(card.Action{Kind: "other", Verb: "x"}); got != "" {
		t.Errorf("SubjectOf(other) = %q, want empty", got)
	}
	if got := card.SweepSubject(42); got != "sweep:42" {
		t.Errorf("SweepSubject(42) = %q", got)
	}
	for subject, want := range map[string]string{"sq:s:abc": "sq", "sweep:42": "sweep", "inc": "inc"} {
		if got := card.Kind(subject); got != want {
			t.Errorf("Kind(%q) = %q, want %q", subject, got, want)
		}
	}
}

func TestPlain(t *testing.T) {
	t.Parallel()
	btn := []card.Button{{Label: "Resolve", Style: card.Primary, ID: "mf1:inc:resolve:x:y"}}
	cases := []struct {
		name string
		c    card.Card
		want string
	}{
		{
			"title body fields",
			card.Card{Title: "T", Body: "B", Fields: []card.Field{{Name: "Root cause", Value: "x"}}, Buttons: btn},
			"T\nB\nRoot cause: x",
		},
		{"body only", card.Card{Body: "B"}, "B"},
		{
			"items and footer",
			card.Card{
				Title:  "T",
				Fields: []card.Field{{Name: "N", Value: "v"}},
				Items:  []card.Item{{Text: "one", Buttons: btn}, {Text: "two"}},
				Footer: "…and 3 more",
			},
			"T\nN: v\n• one\n• two\n…and 3 more",
		},
	}
	for _, tc := range cases {
		if got := tc.c.Plain(); got != tc.want {
			t.Errorf("%s: Plain() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestComponents(t *testing.T) {
	t.Parallel()
	three := []card.Button{{Label: "a"}, {Label: "b"}, {Label: "c"}}
	c := card.Card{
		Title:   "T",
		Body:    "B",
		Items:   []card.Item{{Text: "one", Buttons: three}, {Text: "two", Buttons: three}},
		Buttons: []card.Button{{Label: "Resolve"}},
		Path:    "/incidents/x",
	}
	if got := c.Components(); got != 16 {
		t.Errorf("Components() = %d, want 16", got)
	}
	c.Verbs = []string{"resolve", "rerun", "keep", "unlock", "preview", "refresh"}
	c.Fields = []card.Field{{Name: "N", Value: "v"}}
	c.Footer = "f"
	if got := c.Components(); got != 24 {
		t.Errorf("Components() with verbs = %d, want 24", got)
	}
}
