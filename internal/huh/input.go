package huh

import (
	"charm.land/bubbles/v2/textinput"
)

type Input struct {
	title    string
	prompt   string
	password bool
	value    string
}

func NewInput() *Input {
	return &Input{}
}

func (i *Input) Title(t string) *Input {
	i.title = t
	return i
}

func (i *Input) Prompt(p string) *Input {
	i.prompt = p
	return i
}

func (i *Input) Password(p bool) *Input {
	i.password = p
	return i
}

func (i *Input) Value(v *string) *Input {
	if v != nil {
		i.value = *v
	}
	return i
}

func (i *Input) Model() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = i.prompt
	ti.SetValue(i.value)
	ti.CharLimit = 200
	if i.password {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '*'
	}
	return ti
}
