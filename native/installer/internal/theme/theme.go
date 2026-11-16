// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package theme carries the color tokens and lipgloss styles, ported from
// the onboarding TUI's theme.py. This EKS copy changes the colors only:
// Amazon orange accents over neutral grays, where substrate-gke has
// Electric Sky over Obsidian.
package theme

import "github.com/charmbracelet/lipgloss"

// Color tokens. Every color the wizard draws is named here.
var (
	Orange     = lipgloss.Color("#FF9900") // primary accent: Amazon orange
	OrangeDeep = lipgloss.Color("#EC7211") // secondary accent, a deeper orange
	Mint       = lipgloss.Color("#34D399") // success
	Yellow     = lipgloss.Color("#FFD60A") // warning: a clear yellow, apart from the oranges
	Rose       = lipgloss.Color("#F43F5E") // error
	Gray       = lipgloss.Color("#A3A3A3") // muted text
	Faint      = lipgloss.Color("#525252") // borders, inactive
	Text       = lipgloss.Color("#E8E8E8") // bright text: the selection, the splash
	Highlight  = lipgloss.Color("#5C3300") // selection background, a dark orange
	Ink        = lipgloss.Color("#1F1F1F") // the splash's background
)

var (
	Title    = lipgloss.NewStyle().Bold(true).Foreground(Orange)
	Subtle   = lipgloss.NewStyle().Foreground(Gray)
	Fainted  = lipgloss.NewStyle().Foreground(Faint)
	Accent   = lipgloss.NewStyle().Foreground(OrangeDeep)
	Good     = lipgloss.NewStyle().Foreground(Mint)
	Warning  = lipgloss.NewStyle().Foreground(Yellow)
	Bad      = lipgloss.NewStyle().Foreground(Rose)
	Key      = lipgloss.NewStyle().Bold(true).Foreground(Orange)
	Selected = lipgloss.NewStyle().Bold(true).Foreground(Text).Background(Highlight)

	Panel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Faint).
		Padding(0, 1)

	AccentPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Orange).
			Padding(0, 1)

	ErrorPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(Rose).
			Padding(0, 1)

	CommandLine = lipgloss.NewStyle().Foreground(Mint)
)

// SpinnerFrames are the Braille frames the prototype's doctor rows use.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Glyphs for checklist and step states.
const (
	GlyphDone    = "✓"
	GlyphFail    = "✗"
	GlyphWarn    = "!"
	GlyphPending = "○"
	GlyphActive  = "●"
)

// logoLines is the AGENT SUBSTRATE wordmark, following the prototype's hero
// screen.
var logoLines = []string{
	`▄▀█ █▀▀ █▀▀ █▄░█ ▀█▀   █▀ █░█ █▄▄ █▀ ▀█▀ █▀█ ▄▀█ ▀█▀ █▀▀`,
	`█▀█ █▄█ ██▄ █░▀█ ░█░   ▄█ █▄█ █▄█ ▄█ ░█░ █▀▄ █▀█ ░█░ ██▄`,
}

// gradient interpolated between Orange and OrangeDeep, one stop per logo line.
var gradient = []lipgloss.Color{
	Orange, OrangeDeep,
}

// Logo renders the wordmark with an orange→deeper orange gradient.
func Logo() string {
	out := ""
	for i, line := range logoLines {
		c := gradient[i%len(gradient)]
		out += lipgloss.NewStyle().Foreground(c).Render(line)
		if i < len(logoLines)-1 {
			out += "\n"
		}
	}
	return out
}
