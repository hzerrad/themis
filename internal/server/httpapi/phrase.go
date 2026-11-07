package httpapi

import (
	"regexp"
	"strings"
	"time"
)

// phrase is a line people read, in pieces the page shows each as it should:
// words, a literal cluster value in monospace, or a moment in the reader's
// time zone.
type phrase []piece

type piece struct {
	Text  string    `json:"text,omitempty"`
	Value string    `json:"value,omitempty"`
	Time  time.Time `json:"time,omitzero"`
}

func (p phrase) text(s string) phrase {
	if s == "" {
		return p
	}
	if n := len(p); n > 0 && p[n-1].Text != "" {
		p[n-1].Text += s
		return p
	}
	return append(p, piece{Text: s})
}

func (p phrase) value(s string) phrase {
	if s == "" {
		return p
	}
	return append(p, piece{Value: s})
}

func (p phrase) at(t time.Time) phrase {
	if t.IsZero() {
		return p
	}
	return append(p, piece{Time: t.UTC()})
}

// String is the phrase as plain text, times in RFC 3339.
func (p phrase) String() string {
	var b strings.Builder
	for _, x := range p {
		switch {
		case x.Value != "":
			b.WriteString(x.Value)
		case !x.Time.IsZero():
			b.WriteString(x.Time.Format(time.RFC3339))
		default:
			b.WriteString(x.Text)
		}
	}
	return b.String()
}

// moment finds the times a check writes into its texts, in RFC 3339.
var moment = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

// written is a text a check wrote for people, such as a fact, as a phrase:
// text in backticks is a literal cluster value, and a time in RFC 3339 is a
// moment.
func written(s string) phrase {
	var out phrase
	for i, chunk := range strings.Split(s, "`") {
		if i%2 == 1 {
			out = out.value(chunk)
			continue
		}
		for chunk != "" {
			loc := moment.FindStringIndex(chunk)
			if loc == nil {
				out = out.text(chunk)
				break
			}
			out = out.text(chunk[:loc[0]])
			if t, err := time.Parse(time.RFC3339, chunk[loc[0]:loc[1]]); err == nil {
				out = out.at(t)
			} else {
				out = out.text(chunk[loc[0]:loc[1]])
			}
			chunk = chunk[loc[1]:]
		}
	}
	return out
}
