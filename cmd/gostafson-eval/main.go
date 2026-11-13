// Command gostafson-eval runs the golden cases in evals/ against one or more
// local models and prints pass rates, tool-call errors and speed.
//
//	gostafson-eval -config config.local.toml -models qwen3.5:9b,mistral:7b
//
// Each local model is run through a variant with a 32K context
// ("gostafson-<model>", created on the Ollama server if missing), because
// Ollama's /v1 API ignores per-request context size. -cache also checks
// whether the model reuses Ollama's prompt cache.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ahplummer/gostafson/internal/client"
	"github.com/ahplummer/gostafson/internal/config"
	"github.com/ahplummer/gostafson/internal/evals"
)

func main() {
	var (
		cfgPath  = flag.String("config", "config.local.toml", "config file (profiles, base URLs)")
		casesF   = flag.String("cases", "evals/cases.toml", "cases file")
		fixtures = flag.String("fixtures", "evals/fixtures", "fixtures directory")
		models   = flag.String("models", "", "comma-separated models to test on the local profiles (default: each profile's own model)")
		only     = flag.String("only", "", "run only cases whose name contains this")
		repeat   = flag.Int("n", 1, "runs per case")
		ctxSize  = flag.Int("ctx", 32768, "context size for the model variants; 0 uses models as named")
		cache    = flag.Bool("cache", true, "check prompt-cache reuse per model")
		outDir   = flag.String("out", "var/evals", "directory for the JSONL results")
		timeout  = flag.Duration("timeout", 5*time.Minute, "limit per case")
	)
	flag.Parse()
	if err := run(*cfgPath, *casesF, *fixtures, *models, *only, *repeat, *ctxSize, *cache, *outDir, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfgPath, casesF, fixtures, models, only string, repeat, ctxSize int, cache bool, outDir string, timeout time.Duration) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	cases, err := evals.Load(casesF)
	if err != nil {
		return err
	}
	cases = slices.DeleteFunc(cases, func(c evals.Case) bool { return !strings.Contains(c.Name, only) })
	for _, c := range cases {
		p, ok := cfg.Profiles[c.Profile]
		if !ok {
			return fmt.Errorf("case %s: no profile %q in %s", c.Name, c.Profile, cfgPath)
		}
		if p.Remote() {
			return fmt.Errorf("case %s: profile %q is remote; evals run local models only", c.Name, c.Profile)
		}
	}
	var list []string // empty entry: each profile's own model
	if models == "" {
		list = []string{""}
	} else {
		list = strings.Split(models, ",")
	}

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	outPath := filepath.Join(outDir, time.Now().Format("20060102-150405")+".jsonl")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	var rows []summary
	for _, m := range list {
		m = strings.TrimSpace(m)
		s := summary{model: m}
		for _, c := range cases {
			p := *cfg.Profiles[c.Profile]
			if m != "" {
				p.Model = m
			}
			if s.model == "" {
				s.model = p.Model
			}
			if ctxSize > 0 {
				if p.Model, err = variant(p.BaseURL, p.Model, ctxSize); err != nil {
					return err
				}
			}
			if cache && s.cache == "" {
				s.cache = cacheCheck(p.BaseURL, p.Model, ctxSize)
				fmt.Printf("%s: prompt cache %s\n", s.model, s.cache)
			}
			cl := client.New(p.BaseURL, p.ResolvedAPIKey)
			for range repeat {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				r := evals.Run(ctx, cl, &p, c, fixtures, cfg.Loop.MaxSteps)
				cancel()
				enc.Encode(r)
				s.add(r)
				printResult(r)
			}
		}
		rows = append(rows, s)
	}
	fmt.Println()
	fmt.Printf("%-26s %7s %11s %10s %10s %s\n", "model", "passed", "tool errors", "median step", "mean turn", "prompt cache")
	for _, s := range rows {
		fmt.Printf("%-26s %3d/%-3d %11d %9.1fs %9.1fs %s\n", s.model, s.pass, s.total, s.toolErrs, median(s.steps)/1000, mean(s.turns)/1000, s.cache)
	}
	fmt.Println("\nresults:", outPath)
	return nil
}

func printResult(r evals.Result) {
	mark := "PASS"
	if !r.Pass {
		mark = "FAIL"
	}
	fmt.Printf("  %s %-24s %2d steps %6.1fs  %s\n", mark, r.Case, r.Steps, float64(r.TurnMS)/1000, strings.Join(r.Calls, ", "))
	if r.Err != "" {
		fmt.Printf("       error: %s\n", r.Err)
	}
	for _, f := range r.Failures {
		fmt.Printf("       %s\n", f)
	}
}

type summary struct {
	model, cache string
	pass, total  int
	toolErrs     int
	steps, turns []float64
}

func (s *summary) add(r evals.Result) {
	s.total++
	if r.Pass {
		s.pass++
	}
	s.toolErrs += r.ToolErrors
	for _, ms := range r.StepMS {
		s.steps = append(s.steps, float64(ms))
	}
	s.turns = append(s.turns, float64(r.TurnMS))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	v = slices.Sorted(slices.Values(v))
	return v[len(v)/2]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var t float64
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

// ollamaBase turns ".../v1" into the server's native API base.
func ollamaBase(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// variant returns the name of model with a ctx-token context, creating it on
// the Ollama server if needed. Variants share the model's weights, so they
// take no extra disk. Names already starting with "gostafson-" are used as is.
func variant(baseURL, model string, ctx int) (string, error) {
	if strings.HasPrefix(model, "gostafson-") {
		return model, nil
	}
	name := "gostafson-" + strings.NewReplacer(":", "-", "/", "-").Replace(strings.TrimSuffix(model, ":latest"))
	body, _ := json.Marshal(map[string]any{"model": name, "from": model, "parameters": map[string]any{"num_ctx": ctx}, "stream": false})
	resp, err := http.Post(ollamaBase(baseURL)+"/api/create", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return "", fmt.Errorf("create %s from %s: HTTP %d %s", name, model, resp.StatusCode, e.Error)
	}
	return name, nil
}

// cacheCheck sends the same long prompt twice (the second with one extra
// character) and reports how long the second took to read. A model that
// reuses Ollama's prompt cache reads it in well under a second.
func cacheCheck(baseURL, model string, ctx int) string {
	prompt := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 150) // ~1,500 tokens
	call := func(suffix string) (float64, error) {
		body, _ := json.Marshal(map[string]any{
			"model": model, "stream": false, "think": false,
			"options":  map[string]any{"num_predict": 1},
			"messages": []map[string]string{{"role": "user", "content": prompt + suffix}},
		})
		resp, err := http.Post(ollamaBase(baseURL)+"/api/chat", "application/json", bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		var r struct {
			PromptEvalDuration int64  `json:"prompt_eval_duration"`
			Error              string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Error != "" {
			return 0, fmt.Errorf("HTTP %d %s", resp.StatusCode, r.Error)
		}
		return float64(r.PromptEvalDuration) / 1e9, nil
	}
	first, err := call("")
	if err != nil {
		return "error: " + err.Error()
	}
	second, err := call(" ")
	if err != nil {
		return "error: " + err.Error()
	}
	verdict := "reused"
	if second > first/4 {
		verdict = "NOT reused"
	}
	return fmt.Sprintf("%s (1st read %.2fs, 2nd %.2fs)", verdict, first, second)
}
