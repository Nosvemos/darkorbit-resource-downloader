package app

import (
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// ansiPattern matches CSI and OSC escape sequences so they can be stripped
// from the plain-text log file.
var ansiPattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// printer writes styled output to the console and a plain copy to the
// optional log file.
type printer struct {
	out      io.Writer
	errOut   io.Writer
	file     *os.File
	renderer *lipgloss.Renderer
	errRend  *lipgloss.Renderer
}

func newPrinter(stdout, stderr io.Writer, noColor bool) *printer {
	p := &printer{
		out:      stdout,
		errOut:   stderr,
		renderer: lipgloss.NewRenderer(stdout),
		errRend:  lipgloss.NewRenderer(stderr),
	}
	if noColor {
		p.renderer.SetColorProfile(termenv.Ascii)
		p.errRend.SetColorProfile(termenv.Ascii)
	}
	return p
}

// openLog starts appending a plain-text copy of all output to path.
func (p *printer) openLog(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	p.file = file
	return nil
}

func (p *printer) Close() error {
	if p.file != nil {
		err := p.file.Close()
		p.file = nil
		return err
	}
	return nil
}

func (p *printer) style() lipgloss.Style {
	return p.renderer.NewStyle()
}

func (p *printer) write(s string) {
	_, _ = io.WriteString(p.out, s)
	p.log(s)
}

func (p *printer) log(s string) {
	if p.file != nil {
		_, _ = io.WriteString(p.file, stripANSI(s))
	}
}

func (p *printer) Printf(format string, args ...any) {
	p.write(fmt.Sprintf(format, args...))
}

func (p *printer) Println(args ...any) {
	p.write(fmt.Sprintln(args...))
}

func (p *printer) Title(title string) {
	p.Println(p.style().Bold(true).Foreground(lipgloss.Color("205")).Render(title))
}

func (p *printer) Section(title string) {
	p.Println(p.style().Bold(true).Foreground(lipgloss.Color("69")).Render(title))
}

func (p *printer) Status(label, message string) {
	style := p.style().Bold(true).Padding(0, 1)
	switch label {
	case "OK":
		style = style.Foreground(lipgloss.Color("42"))
	case "FAIL", "HASH":
		style = style.Foreground(lipgloss.Color("196"))
	case "WARN":
		style = style.Foreground(lipgloss.Color("214"))
	case "FETCH":
		style = style.Foreground(lipgloss.Color("81"))
	case "DOWNLOAD":
		style = style.Foreground(lipgloss.Color("220"))
	default:
		style = style.Foreground(lipgloss.Color("250"))
	}
	p.Println(style.Render(label) + " " + message)
}

func (p *printer) Warnf(format string, args ...any) {
	p.Status("WARN", fmt.Sprintf(format, args...))
}

func (p *printer) KV(key string, value any) {
	p.Println(p.style().Foreground(lipgloss.Color("244")).Render(key+":") + " " + fmt.Sprint(value))
}

// Errorf writes an error line to stderr and the log file.
func (p *printer) Errorf(format string, args ...any) {
	label := p.errRend.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Render("error:")
	msg := label + " " + fmt.Sprintf(format, args...) + "\n"
	_, _ = io.WriteString(p.errOut, msg)
	p.log(msg)
}
