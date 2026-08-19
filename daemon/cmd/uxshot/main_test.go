package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestScenariosRenderDeterministically(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	seenNames := map[string]bool{}
	seenFiles := map[string]bool{}

	for _, shot := range scenarios {
		shot := shot
		t.Run(shot.name, func(t *testing.T) {
			if seenNames[shot.name] {
				t.Fatalf("duplicate scenario name %q", shot.name)
			}
			seenNames[shot.name] = true
			if seenFiles[shot.filename] {
				t.Fatalf("duplicate scenario filename %q", shot.filename)
			}
			seenFiles[shot.filename] = true

			view, err := formView(shot.form())
			if err != nil {
				t.Fatal(err)
			}
			first, err := ansiToSVG(shot.name, view)
			if err != nil {
				t.Fatal(err)
			}
			second, err := ansiToSVG(shot.name, view)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("rendering the same form produced different SVG output")
			}
			if !strings.Contains(string(first), `<svg xmlns="http://www.w3.org/2000/svg"`) {
				t.Fatal("output does not contain an SVG root")
			}
		})
	}
}

func TestSelectScenariosRejectsUnknownName(t *testing.T) {
	if _, err := selectScenarios("missing"); err == nil {
		t.Fatal("selectScenarios accepted an unknown scenario")
	}
}
