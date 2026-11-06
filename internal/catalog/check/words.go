package check

import (
	"strings"
	"unicode"
)

// Says is what the check named says it found when it answered v, in its own
// plain words, to go inside a sentence: "an image can't be pulled". True
// reads as its description; false as what it finds then, or its description
// with "not so"; unknown as its topic being unknown. People read it, so a
// check's name appears only for one l doesn't describe.
func (l List) Says(name string, v Truth) string {
	c, ok := l.Get(name)
	if !ok || c.Description == "" {
		return name + " is " + v.String()
	}
	switch v {
	case True:
		return lowerFirst(c.Description)
	case False:
		if c.WhenFalse != "" {
			return lowerFirst(c.WhenFalse)
		}
		return "not so that " + lowerFirst(c.Description)
	case Unknown:
	}
	topic := c.Topic
	if topic == "" {
		topic = c.Description
	}
	return lowerFirst(topic) + " is unknown"
}

// Found is what r found, as Says puts it, followed by its reason in
// brackets: "an image can't be pulled (inventory-…/app can't pull …)".
func (l List) Found(r Result) string {
	what := l.Says(r.Name, r.Value)
	if why := strings.TrimSuffix(r.Reason, "."); why != "" {
		what += " (" + why + ")"
	}
	return what
}

// Findings says what results found, as Found puts each, in one sentence:
// "The checks found that an image can't be pulled (…) and that …". It is
// empty for no results.
func (l List) Findings(results []Result) string {
	found := make([]string, 0, len(results))
	for _, r := range results {
		found = append(found, "that "+l.Found(r))
	}
	switch n := len(found); n {
	case 0:
		return ""
	case 1:
		return "The checks found " + found[0] + "."
	default:
		return "The checks found " + strings.Join(found[:n-1], ", ") + " and " + found[n-1] + "."
	}
}

// lowerFirst puts a check's words inside a sentence: "An image can't be
// pulled" reads "an image can't be pulled". A first word in capitals, such
// as CPU, stays as it is.
func lowerFirst(s string) string {
	r := []rune(s)
	if len(r) > 1 && unicode.IsUpper(r[1]) {
		return s
	}
	if len(r) > 0 {
		r[0] = unicode.ToLower(r[0])
	}
	return string(r)
}
