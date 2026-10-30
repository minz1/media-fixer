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
	rowSize       = 5
)

var (
	ErrUnknownID = errors.New("unknown button id")
	ErrUntracked = errors.New("card sent as plain text, not tracked")
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

func (c Card) Components() int {
	n := 1
	for _, s := range []string{c.Title, c.Body, c.Footer} {
		if s != "" {
			n++
		}
	}
	if len(c.Fields) > 0 {
		n++
	}
	for _, it := range c.Items {
		n++
		if len(it.Buttons) > 0 {
			n += 1 + len(it.Buttons)
		}
	}
	buttons := max(len(c.Buttons), len(c.Verbs))
	if c.Path != "" {
		buttons++
	}
	return n + buttons + (buttons+rowSize-1)/rowSize
}
