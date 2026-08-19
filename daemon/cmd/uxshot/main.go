// Command uxshot renders deterministic screenshots from agentmux's real Huh
// form builders using synthetic data. It never dials a daemon or reads local
// user, host, filesystem, credential, or session state.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/m-rk/agentmux/daemon/internal/wizardui"
)

const (
	shotWidth   = 784
	shotHeight  = 536
	shotColumns = 78
	shotRows    = 29
	formHeight  = 27 // Huh adds its help line below the group viewport.
	padding     = 18.0
	fontSize    = 16.0
)

type scenario struct {
	name     string
	filename string
	form     func() *huh.Form
}

var scenarios = []scenario{
	{
		name:     "selection",
		filename: "tui-wizard-selection.svg",
		form: func() *huh.Form {
			host, agent := "laptop", "claude-code"
			return wizardui.NewSelectionForm([]string{"laptop", "homelab"}, &host, &agent)
		},
	},
	{
		name:     "claude",
		filename: "tui-wizard-claude.svg",
		form: func() *huh.Form {
			details := &wizardui.Details{
				Instance: "data-import",
				RunUser:  "dev",
				HostName: "laptop",
			}
			return huh.NewForm(wizardui.DetailsGroups("claude-code", details, false)[0])
		},
	},
	{
		name:     "provider",
		filename: "tui-wizard-provider.svg",
		form: func() *huh.Form {
			details := &wizardui.Details{Provider: "ollama", Model: "gpt-oss:20b-cloud"}
			return huh.NewForm(wizardui.DetailsGroups("zero", details, false)[1])
		},
	},
	{
		name:     "kilo-custom",
		filename: "tui-wizard-kilo-custom.svg",
		form: func() *huh.Form {
			details := &wizardui.Details{
				Provider:          "gateway",
				Model:             "code-large",
				ProviderBaseURL:   "https://gateway.example/v1",
				ProviderAPIKeyEnv: "GATEWAY_API_KEY",
			}
			return huh.NewForm(wizardui.DetailsGroups("kilo", details, false)[2])
		},
	},
	{
		name:     "compact",
		filename: "tui-wizard-compact.svg",
		form: func() *huh.Form {
			details := &wizardui.Details{}
			return huh.NewForm(wizardui.DetailsGroups("claude-code", details, false)[1])
		},
	},
}

func main() {
	scenarioName := flag.String("scenario", "all", "scenario to render: all, selection, claude, provider, kilo-custom, or compact")
	outputDir := flag.String("output-dir", "../docs/design/img", "directory for generated SVG files")
	check := flag.Bool("check", false, "verify checked-in screenshots match generated output")
	list := flag.Bool("list", false, "list available scenarios")
	flag.Parse()

	if *list {
		for _, shot := range scenarios {
			fmt.Println(shot.name)
		}
		return
	}

	selected, err := selectScenarios(*scenarioName)
	if err != nil {
		fatal(err)
	}
	if !*check {
		if err := os.MkdirAll(*outputDir, 0o755); err != nil {
			fatal(fmt.Errorf("creating output directory: %w", err))
		}
	}

	lipgloss.SetColorProfile(termenv.TrueColor)
	for _, shot := range selected {
		ansi, err := formView(shot.form())
		if err != nil {
			fatal(fmt.Errorf("rendering %s form: %w", shot.name, err))
		}
		svg, err := ansiToSVG(shot.name, ansi)
		if err != nil {
			fatal(fmt.Errorf("rendering %s SVG: %w", shot.name, err))
		}
		path := filepath.Join(*outputDir, shot.filename)
		if err := writeOrCheck(path, svg, *check); err != nil {
			fatal(err)
		}
		fmt.Printf("%s %s\n", map[bool]string{true: "checked", false: "wrote"}[*check], path)
	}
}

func selectScenarios(name string) ([]scenario, error) {
	if name == "all" {
		return scenarios, nil
	}
	for _, shot := range scenarios {
		if shot.name == name {
			return []scenario{shot}, nil
		}
	}
	return nil, fmt.Errorf("unknown scenario %q; use -list to see valid names", name)
}

func formView(form *huh.Form) (string, error) {
	form.WithWidth(shotColumns).WithHeight(formHeight)
	if cmd := form.Init(); cmd != nil {
		form.Update(cmd())
	}
	form.Update(tea.WindowSizeMsg{Width: shotColumns, Height: formHeight})
	view := strings.ReplaceAll(form.View(), "\r", "")
	if rows := strings.Count(view, "\n") + 1; rows > shotRows {
		return "", fmt.Errorf("form uses %d rows; screenshot supports %d", rows, shotRows)
	}
	return view, nil
}

type color struct{ r, g, b uint8 }

var (
	defaultForeground = color{0xc8, 0xc8, 0xc8}
	defaultBackground = color{0x0d, 0x0d, 0x0f}
)

type textStyle struct {
	foreground color
	background color
	bold       bool
	reverse    bool
}

type styledRun struct {
	row, column int
	text        string
	style       textStyle
}

func ansiToSVG(scenarioName, input string) ([]byte, error) {
	runs, rows, err := parseANSI(input)
	if err != nil {
		return nil, err
	}
	if rows > shotRows {
		return nil, fmt.Errorf("rendered view uses %d rows; maximum is %d", rows, shotRows)
	}

	cellWidth := (shotWidth - 2*padding) / shotColumns
	lineHeight := (shotHeight - 2*padding) / shotRows
	var out bytes.Buffer
	fmt.Fprintln(&out, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprintf(&out, "<!-- Code generated by daemon/cmd/uxshot for scenario %s. DO NOT EDIT. -->\n", html.EscapeString(scenarioName))
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="agentmux new %s wizard">`+"\n", shotWidth, shotHeight, shotWidth, shotHeight, html.EscapeString(scenarioName))
	fmt.Fprintf(&out, `  <rect width="100%%" height="100%%" fill="%s"/>`+"\n", defaultBackground.hex())
	fmt.Fprintln(&out, `  <g font-family="ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace" font-size="16" font-variant-ligatures="none">`)
	for _, run := range runs {
		foreground, background := run.style.foreground, run.style.background
		if run.style.reverse {
			foreground, background = background, foreground
			x := padding + float64(run.column)*cellWidth
			y := padding + float64(run.row)*lineHeight
			fmt.Fprintf(&out, `    <rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"/>`+"\n", x, y, float64(utf8.RuneCountInString(run.text))*cellWidth, lineHeight, background.hex())
		}
		x := padding + float64(run.column)*cellWidth
		y := padding + float64(run.row)*lineHeight + fontSize - 1
		weight := "400"
		if run.style.bold {
			weight = "700"
		}
		fmt.Fprintf(&out, `    <text x="%.2f" y="%.2f" fill="%s" font-weight="%s">%s</text>`+"\n", x, y, foreground.hex(), weight, html.EscapeString(run.text))
	}
	fmt.Fprintln(&out, "  </g>")
	fmt.Fprintln(&out, "</svg>")
	return out.Bytes(), nil
}

func parseANSI(input string) ([]styledRun, int, error) {
	style := textStyle{foreground: defaultForeground, background: defaultBackground}
	row, column, maxRow := 0, 0, 0
	var runs []styledRun
	for i := 0; i < len(input); {
		if input[i] == 0x1b {
			if i+1 >= len(input) || input[i+1] != '[' {
				return nil, 0, fmt.Errorf("unsupported escape sequence at byte %d", i)
			}
			end := strings.IndexByte(input[i+2:], 'm')
			if end < 0 {
				return nil, 0, fmt.Errorf("unterminated SGR sequence at byte %d", i)
			}
			params := input[i+2 : i+2+end]
			if err := applySGR(&style, params); err != nil {
				return nil, 0, err
			}
			i += end + 3
			continue
		}
		r, size := utf8.DecodeRuneInString(input[i:])
		i += size
		switch r {
		case '\n':
			row++
			column = 0
			if row > maxRow {
				maxRow = row
			}
		case '\r':
			continue
		default:
			if r != ' ' || style.reverse {
				appendRun(&runs, row, column, string(r), style)
			}
			column++
		}
	}
	return runs, maxRow + 1, nil
}

func appendRun(runs *[]styledRun, row, column int, text string, style textStyle) {
	if len(*runs) > 0 {
		last := &(*runs)[len(*runs)-1]
		if last.row == row && last.column+utf8.RuneCountInString(last.text) == column && last.style == style {
			last.text += text
			return
		}
	}
	*runs = append(*runs, styledRun{row: row, column: column, text: text, style: style})
}

func applySGR(style *textStyle, params string) error {
	if params == "" {
		params = "0"
	}
	parts := strings.Split(params, ";")
	values := make([]int, len(parts))
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("invalid SGR parameter %q", part)
		}
		values[i] = value
	}
	for i := 0; i < len(values); i++ {
		switch values[i] {
		case 0:
			*style = textStyle{foreground: defaultForeground, background: defaultBackground}
		case 1:
			style.bold = true
		case 7:
			style.reverse = true
		case 22:
			style.bold = false
		case 27:
			style.reverse = false
		case 38, 48:
			foreground := values[i] == 38
			parsed, consumed, err := parseSGRColor(values[i+1:])
			if err != nil {
				return err
			}
			if foreground {
				style.foreground = parsed
			} else {
				style.background = parsed
			}
			i += consumed
		case 39:
			style.foreground = defaultForeground
		case 49:
			style.background = defaultBackground
		default:
			return fmt.Errorf("unsupported SGR parameter %d", values[i])
		}
	}
	return nil
}

func parseSGRColor(values []int) (color, int, error) {
	if len(values) < 2 {
		return color{}, 0, errors.New("incomplete SGR color")
	}
	switch values[0] {
	case 2:
		if len(values) < 4 {
			return color{}, 0, errors.New("incomplete true-color SGR sequence")
		}
		return color{uint8(values[1]), uint8(values[2]), uint8(values[3])}, 4, nil
	case 5:
		return xtermColor(values[1]), 2, nil
	default:
		return color{}, 0, fmt.Errorf("unsupported SGR color mode %d", values[0])
	}
}

func xtermColor(index int) color {
	base := [...]color{
		{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0},
		{0, 0, 128}, {128, 0, 128}, {0, 128, 128}, {192, 192, 192},
		{128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
		{0, 0, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
	}
	if index >= 0 && index < len(base) {
		return base[index]
	}
	if index >= 16 && index <= 231 {
		value := index - 16
		steps := [...]uint8{0, 95, 135, 175, 215, 255}
		return color{steps[value/36], steps[(value/6)%6], steps[value%6]}
	}
	if index >= 232 && index <= 255 {
		gray := uint8(8 + (index-232)*10)
		return color{gray, gray, gray}
	}
	return defaultForeground
}

func (c color) hex() string {
	return fmt.Sprintf("#%02x%02x%02x", c.r, c.g, c.b)
}

func writeOrCheck(path string, data []byte, check bool) error {
	if check {
		existing, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("%s is stale; run scripts/generate-ux-screenshots.sh", path)
		}
		return nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "uxshot:", err)
	os.Exit(1)
}
