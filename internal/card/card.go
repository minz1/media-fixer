package card

import (
	"context"
	"errors"
)

const (
	MaxIDLen      = 100
	MaxComponents = 40
	Reserve       = 3
	MaxBuilt      = MaxComponents - Reserve
	MaxText       = 4000
	MaxPlain      = 2000
	RowSize       = 5

	RestartingNotice = "media-fixer is restarting; try again in a minute."
)

var (
	ErrUnknownID  = errors.New("unknown button id")
	ErrUntracked  = errors.New("card sent as plain text, not tracked")
	ErrRestarting = errors.New("media-fixer is restarting")
)

type Tone int

const (
	Info Tone = iota
	Success
	Warn
	Alert
)

type Style int

const (
	Primary Style = iota
	Secondary
	Danger
	Link
)

type Button struct {
	Label string `json:"label"`
	Style Style  `json:"style"`
	ID    string `json:"id,omitempty"`
	Path  string `json:"path,omitempty"`
}

type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Item struct {
	Subject string   `json:"subject,omitempty"`
	Text    string   `json:"text"`
	Buttons []Button `json:"buttons,omitempty"`
}

type Card struct {
	Subject string   `json:"subject,omitempty"`
	Tone    Tone     `json:"tone"`
	Title   string   `json:"title,omitempty"`
	Body    string   `json:"body,omitempty"`
	Fields  []Field  `json:"fields,omitempty"`
	Items   []Item   `json:"items,omitempty"`
	Footer  string   `json:"footer,omitempty"`
	Buttons []Button `json:"buttons,omitempty"`
	Verbs   []string `json:"verbs,omitempty"`
	Path    string   `json:"path,omitempty"`
}

type Update struct {
	Status   string   `json:"status,omitempty"`
	Sections []Field  `json:"sections,omitempty"`
	Buttons  []Button `json:"buttons,omitempty"`
	Live     bool     `json:"live,omitempty"`
	Tone     *Tone    `json:"tone,omitempty"`
}

type Result struct {
	Line    string
	Confirm []Button
}

type Action struct {
	Kind string
	Verb string
	Args []string
}

type (
	Handler  func(ctx context.Context, a Action) (Result, error)
	Renderer func(ctx context.Context, subject string) (Update, bool, error)
)

// Components predicts the built component count: exact with a public URL, an upper bound without.
func (c Card) Components() int {
	n := 1
	header := 0
	switch {
	case c.Title != "" && c.Path != "":
		header += 3
	case c.Title != "":
		header++
	}
	if c.Body != "" {
		header++
	}
	if len(c.Fields) > 0 {
		header++
	}
	if c.Footer != "" {
		n++
	}
	for _, it := range c.Items {
		n += 1 + len(it.Buttons) + (len(it.Buttons)+RowSize-1)/RowSize
	}
	n += max(len(c.Items)-1, 0)
	buttons := max(len(c.Buttons), len(c.Verbs))
	if c.Title == "" && c.Path != "" {
		buttons++
	}
	if header > 0 && (len(c.Items) > 0 || buttons > 0) {
		n++
	}
	return n + header + buttons + (buttons+RowSize-1)/RowSize
}
