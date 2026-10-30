package card

import "strings"

func (c Card) Plain() string {
	var lines []string
	for _, s := range []string{c.Title, c.Body} {
		if s != "" {
			lines = append(lines, s)
		}
	}
	for _, f := range c.Fields {
		lines = append(lines, f.Name+": "+f.Value)
	}
	for _, it := range c.Items {
		lines = append(lines, "• "+it.Text)
	}
	if c.Footer != "" {
		lines = append(lines, c.Footer)
	}
	return strings.Join(lines, "\n")
}
