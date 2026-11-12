package transport

import (
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
)

// ANSI styling for the CLI. Only used when stdout is a terminal and NO_COLOR
// is unset; tests and pipes get plain text.

const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	italic  = "\033[3m"
	red     = "\033[38;5;203m"
	yellow  = "\033[38;5;221m"
	cyan    = "\033[38;5;117m"
	accent  = "\033[38;5;209m" // warm orange
	accent2 = "\033[38;5;176m" // soft violet
	gray    = "\033[38;5;245m"
)

// isTerminal reports whether f is a terminal that should get color.
func isTerminal(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

var bannerLines = []string{
	"┏━╸┏━┓┏━┓╺┳╸┏━┓┏━╸┏━┓┏━┓┏┓╻",
	"┃╺┓┃ ┃┗━┓ ┃ ┣━┫┣╸ ┗━┓┃ ┃┃┗┫",
	"┗━┛┗━┛┗━┛ ╹ ╹ ╹╹  ┗━┛┗━┛╹ ╹",
}

// gradient runs left to right across the banner: orange to violet.
var gradient = []int{209, 209, 210, 211, 211, 212, 212, 176, 177, 141}

func banner(color bool, version string) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, line := range bannerLines {
		b.WriteString("  ")
		if !color {
			b.WriteString(line + "\n")
			continue
		}
		runes := []rune(line)
		for i, r := range runes {
			c := gradient[i*len(gradient)/len(runes)]
			fmt.Fprintf(&b, "\033[38;5;%dm%c", c, r)
		}
		b.WriteString(reset + "\n")
	}
	tag := fmt.Sprintf("  a small local-first agent · %s", version)
	hint := "  /help for commands · @tag to pick a model · think=true to reason"
	if color {
		tag = gray + italic + tag + reset
		hint = gray + hint + reset
	}
	b.WriteString(tag + "\n" + hint + "\n")
	return b.String()
}

// verbs shown by the spinner while a turn runs.
var verbs = []string{
	"Pontificating", "Swirling", "Ruminating", "Percolating", "Noodling",
	"Cogitating", "Marinating", "Mulling", "Simmering", "Conjuring",
	"Untangling", "Deliberating", "Musing", "Brewing", "Tinkering",
	"Woolgathering", "Spelunking", "Divining", "Rummaging", "Puzzling",
	"Whittling", "Daydreaming", "Concocting", "Fermenting", "Moseying",
}

func randomVerb(not string) string {
	for {
		if v := verbs[rand.IntN(len(verbs))]; v != not {
			return v
		}
	}
}

var spinFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// footer matches the status line main appends: "[profile | model | 1.2s | 34 tok]".
var footer = regexp.MustCompile(`^\[[^\]]*\|[^\]]*\]$`)

// render colors a reply: code fences cyan, "error:" lines red, the footer dim.
func render(text string) string {
	lines := strings.Split(text, "\n")
	inFence := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "```"):
			inFence = !inFence
			lines[i] = gray + l + reset
		case inFence:
			lines[i] = cyan + l + reset
		case strings.HasPrefix(t, "error:"):
			lines[i] = red + l + reset
		case i == len(lines)-1 && footer.MatchString(t):
			lines[i] = gray + dim + l + reset
		default:
			lines[i] = highlightInline(l)
		}
	}
	return strings.Join(lines, "\n")
}

var inlineCode = regexp.MustCompile("`[^`]+`")
var boldText = regexp.MustCompile(`\*\*[^*]+\*\*`)

func highlightInline(l string) string {
	l = inlineCode.ReplaceAllStringFunc(l, func(s string) string { return cyan + s + reset })
	return boldText.ReplaceAllStringFunc(l, func(s string) string { return bold + strings.Trim(s, "*") + reset })
}
