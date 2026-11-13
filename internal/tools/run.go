package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The run tool executes one command from a fixed list (PRD D31). There is no
// shell: the model gives a command name and an argument list, and both are
// checked against that command's spec before anything starts. Commands not on
// the list are refused, never offered for approval. Each command runs:
//   - from a fixed absolute path (no PATH lookup), in the root, stdin empty;
//   - with an environment of PATH, HOME (the root), LANG and TMPDIR only;
//   - under sandbox-exec: no network, no writes outside the root, and no
//     reads under /Users except the root (so not the agent's secrets file);
//   - with a 30 s limit that kills its whole process group, output capped.

const runTimeout = 30 * time.Second

// argKind says what a flag's value or a positional argument is.
type argKind int

const (
	none    argKind = iota // a flag without a value
	text                   // free text (a pattern, a count, a message)
	pathArg                // a path, checked like the file tools' paths
)

type spec struct {
	names      []string           // program locations, tried in order
	form       string             // shown to the model
	flags      map[string]argKind // allowed flags; anything else is refused
	combine    bool               // "-la" means "-l -a" (not for find)
	pattern    bool               // the first positional is text, not a path
	noPatIf    string             // ...unless this flag is present (rg --files)
	minPos     int                // positional arguments, after any pattern
	maxPos     int                // -1: no limit
	require    string             // a flag that must be present (node --check)
	approve    bool               // asks the owner first
	git        bool               // a git subcommand: hardened, see gitArgv
	revsOK     bool               // positionals may be git revisions
	gitSub     string             // the subcommand name
	prepend    []string           // fixed arguments after the subcommand
	pathsFirst bool               // paths go before the flags (find)
	noDashDash bool               // the program doesn't take "--" (jq)
}

func at(name string) []string {
	return []string{"/opt/homebrew/bin/" + name, "/usr/bin/" + name, "/bin/" + name}
}

func flags(kv ...any) map[string]argKind {
	m := map[string]argKind{}
	for i := 0; i < len(kv); i += 2 {
		for _, f := range strings.Fields(kv[i].(string)) {
			m[f] = kv[i+1].(argKind)
		}
	}
	return m
}

// commands is the allowlist. Keys are what the model passes as "command";
// git subcommands are "git status", "git diff" and so on.
var commands = map[string]spec{
	"ls":   {names: at("ls"), form: "ls [-l -a -1 -h -t -S -r] [path...]", flags: flags("-l -a -1 -h -t -S -r", none), combine: true, maxPos: -1},
	"tree": {names: at("tree"), form: "tree [-a -d] [-L depth] [-I pattern] [path]", flags: flags("-a -d --noreport", none, "-L -I", text), maxPos: 1},
	"wc":   {names: at("wc"), form: "wc [-l -w -c -m] path...", flags: flags("-l -w -c -m", none), combine: true, minPos: 1, maxPos: -1},
	"head": {names: at("head"), form: "head [-n lines | -c bytes] path...", flags: flags("-n -c", text), minPos: 1, maxPos: -1},
	"tail": {names: at("tail"), form: "tail [-n lines | -c bytes] path...", flags: flags("-n -c", text), minPos: 1, maxPos: -1},
	"diff": {names: at("diff"), form: "diff [-u -q -r -b -w -i] path path", flags: flags("-u -q -r -b -w -i", none), combine: true, minPos: 2, maxPos: 2},
	"grep": {names: at("grep"), form: "grep [-r -n -i -E -F -l -c -w -v -o] [-A/-B/-C n] [-m n] [--include glob] pattern [path...]",
		flags:   flags("-r -n -i -E -F -l -c -w -v -o", none, "-A -B -C -m --include --exclude", text),
		combine: true, pattern: true, maxPos: -1},
	"rg": {names: at("rg"), form: "rg [-n -i -l -c -w -v -F -S --hidden] [-g glob] [-t type] [-A/-B/-C n] pattern [path...]; rg --files [path]",
		flags:   flags("-n -i -l -c -w -v -F -S --hidden --files", none, "-g -t -A -B -C -m", text),
		combine: true, pattern: true, noPatIf: "--files", maxPos: -1},
	"find": {names: at("find"), pathsFirst: true, form: "find [path...] [-name/-iname pattern] [-type f|d] [-maxdepth/-mindepth n] [-size n] [-mtime n] [-newer path] [-not] [-o] [-print]",
		flags: flags("-print -not -o", none, "-name -iname -type -maxdepth -mindepth -size -mtime", text, "-newer", pathArg), maxPos: -1},
	"file":  {names: at("file"), form: "file [-b] path...", flags: flags("-b", none), minPos: 1, maxPos: -1},
	"du":    {names: at("du"), form: "du [-s -h -c] [-d depth] [path...]", flags: flags("-s -h -c", none, "-d", text), combine: true, maxPos: -1},
	"jq":    {names: at("jq"), noDashDash: true, form: "jq [-r -c -e -S] filter path...", flags: flags("-r -c -e -S", none), combine: true, pattern: true, minPos: 1, maxPos: -1},
	"node":  {names: at("node"), form: "node --check path.js (syntax check; doesn't run the file)", flags: flags("--check", none), require: "--check", minPos: 1, maxPos: 1},
	"mkdir": {names: at("mkdir"), form: "mkdir [-p] path...", flags: flags("-p", none), minPos: 1, maxPos: -1},

	"rm": {names: at("rm"), form: "rm path... (asks the owner; files only)", minPos: 1, maxPos: -1, approve: true},
	"mv": {names: at("mv"), form: "mv from to (asks the owner)", minPos: 2, maxPos: 2, approve: true},
	"cp": {names: at("cp"), form: "cp [-R] from to (asks the owner)", flags: flags("-R", none), minPos: 2, maxPos: 2, approve: true},

	"git status": {names: at("git"), git: true, gitSub: "status", form: "git status [-s -b]", flags: flags("-s --short -b --porcelain", none), combine: true},
	"git diff": {names: at("git"), git: true, gitSub: "diff", form: "git diff [--stat --name-only --cached] [rev...] [-- path...]",
		flags: flags("--stat --name-only --cached --staged", none), revsOK: true, maxPos: -1, prepend: []string{"--no-ext-diff", "--no-textconv"}},
	"git log": {names: at("git"), git: true, gitSub: "log", form: "git log [--oneline --stat -p] [-n count] [rev] [-- path...]",
		flags: flags("--oneline --stat -p", none, "-n", text), revsOK: true, maxPos: -1, prepend: []string{"--no-ext-diff", "--no-textconv"}},
	"git show": {names: at("git"), git: true, gitSub: "show", form: "git show [--stat --name-only] [rev]",
		flags: flags("--stat --name-only", none), revsOK: true, maxPos: -1, prepend: []string{"--no-ext-diff", "--no-textconv"}},
	"git init":   {names: at("git"), git: true, gitSub: "init", form: "git init (asks the owner)", approve: true},
	"git add":    {names: at("git"), git: true, gitSub: "add", form: "git add path... (asks the owner)", flags: flags("-A", none), maxPos: -1, approve: true},
	"git commit": {names: at("git"), git: true, gitSub: "commit", form: "git commit [-a] -m message (asks the owner)", flags: flags("-a", none, "-m", text), require: "-m", approve: true},
}

// RunTool returns the run tool. The description is short on purpose: it is
// re-read at every step, and a model that doesn't reuse the prompt cache pays
// for each token every time. A refused call's error lists the allowed form.
func RunTool() Tool {
	desc := "Run one command from a fixed list in the working directory. This is the only way to run commands: " +
		"there is no rm, git or ls tool, so call run, e.g. {\"command\":\"wc\",\"args\":[\"-l\",\"app.js\"]} or {\"command\":\"git status\"}. " +
		"No shell: no pipes, redirects, globs or variables. " +
		"Read-only: ls, tree, wc, head, tail, diff, grep, rg, find, file, du, jq, node --check (syntax only), mkdir, git status/diff/log/show. " +
		"Ask the user first, automatically: rm, mv, cp, git init/add/commit (commit needs -m). " +
		"Nothing else is available. If a flag is refused, the error shows the allowed ones."
	return Tool{Def: def("run", desc, runSchema), Run: run, Confirm: confirmRun, Check: checkRun}
}

func mapsKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

const runSchema = `{"type":"object","properties":{
	"command":{"type":"string","description":"Command name from the list, e.g. \"ls\", \"rg\", \"git status\"."},
	"args":{"type":"array","items":{"type":"string"},"description":"Arguments, one per item, e.g. [\"-n\", \"TODO\", \"src\"]."}},"required":["command"]}`

type runArgs struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// checked is a command that passed its spec: the program and full argv.
type checked struct {
	spec spec
	prog string
	argv []string // without the program
}

var gitRev = regexp.MustCompile(`^[A-Za-z0-9._/~^@{}][A-Za-z0-9._/~^@{}-]*$`)

// checkRun is the guard's pre-check: an error means the call is refused.
func checkRun(root *os.Root, raw json.RawMessage) error {
	_, err := prepare(root, raw)
	return err
}

func prepare(root *os.Root, raw json.RawMessage) (checked, error) {
	var a runArgs
	if err := decode(raw, &a); err != nil {
		return checked{}, err
	}
	// Models often put arguments in the command ("git add ."): split it into
	// words (quotes group, nothing else is interpreted) and put the extra
	// words before args. "git" + ["status", ...] works too.
	words, err := splitWords(a.Command)
	if err != nil || len(words) == 0 {
		return checked{}, fmt.Errorf("command %q: %v", a.Command, cmp.Or(err, errors.New("empty")))
	}
	a.Args = append(words[1:], a.Args...)
	name := words[0]
	if name == "git" && len(a.Args) > 0 {
		name, a.Args = "git "+a.Args[0], a.Args[1:]
	}
	sp, ok := commands[name]
	if !ok {
		return checked{}, fmt.Errorf("command %q is not allowed; allowed: %s", a.Command, strings.Join(mapsKeys(commands), ", "))
	}
	prog := ""
	for _, p := range sp.names {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			prog = p
			break
		}
	}
	if prog == "" {
		return checked{}, fmt.Errorf("%s is not installed on this machine", strings.Fields(name)[0])
	}
	argv, err := checkArgs(root, sp, a.Args)
	if err != nil {
		return checked{}, fmt.Errorf("%s: %w (allowed: %s)", name, err, sp.form)
	}
	return checked{spec: sp, prog: prog, argv: argv}, nil
}

func checkArgs(root *os.Root, sp spec, args []string) ([]string, error) {
	var out, pos []string
	seen := map[string]bool{}
	dashdash := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.ContainsRune(a, 0) {
			return nil, errors.New("NUL in argument")
		}
		if dashdash || a == "-" || !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		if a == "--" {
			dashdash = true
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "--") {
			name, hasVal = a, false
		}
		kind, ok := sp.flags[name]
		if !ok && sp.combine && !strings.HasPrefix(a, "--") && len(a) > 2 {
			for _, c := range a[1:] { // "-la": every letter a flag without a value
				if k, ok := sp.flags["-"+string(c)]; !ok || k != none {
					return nil, fmt.Errorf("flag %q not allowed", a)
				}
				seen["-"+string(c)] = true
			}
			out = append(out, a)
			continue
		}
		if !ok {
			return nil, fmt.Errorf("flag %q not allowed", a)
		}
		seen[name] = true
		if kind == none {
			if hasVal {
				return nil, fmt.Errorf("flag %s takes no value", name)
			}
			out = append(out, a)
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %s needs a value", name)
			}
			i++
			val = args[i]
		}
		if kind == pathArg {
			if err := checkPath(root, val, sp.approve); err != nil {
				return nil, err
			}
		}
		out = append(out, name, val)
	}
	if sp.require != "" && !seen[sp.require] {
		return nil, fmt.Errorf("%s is required", sp.require)
	}
	var pat []string
	if sp.pattern && !(sp.noPatIf != "" && seen[sp.noPatIf]) {
		if len(pos) == 0 {
			return nil, errors.New("a pattern is required")
		}
		switch {
		case !strings.HasPrefix(pos[0], "-"):
			pat = []string{pos[0]}
		case sp.noDashDash:
			return nil, fmt.Errorf("%q can't start with -", pos[0])
		default:
			pat = []string{"--", pos[0]} // so a pattern can't be read as a flag
		}
		pos = pos[1:]
	}
	if len(pos) < sp.minPos || (sp.maxPos >= 0 && len(pos) > sp.maxPos) {
		return nil, fmt.Errorf("wrong number of arguments (%d)", len(pos))
	}
	var revs, paths []string
	for _, p := range pos {
		if sp.revsOK && !dashdash && !exists(root, p) {
			if !gitRev.MatchString(p) {
				return nil, fmt.Errorf("%q is neither a file here nor a git revision", p)
			}
			revs = append(revs, p)
			continue
		}
		if err := checkPath(root, p, sp.approve); err != nil {
			return nil, err
		}
		if strings.HasPrefix(p, "-") {
			p = "./" + p // a path, never a flag
		}
		paths = append(paths, p)
	}
	if sp.pathsFirst {
		return append(paths, out...), nil
	}
	out = append(out, pat...)
	out = append(out, revs...)
	if sp.git && len(paths) > 0 {
		out = append(out, "--")
	}
	return append(out, paths...), nil
}

// splitWords splits s at spaces; "double" or 'single' quotes keep spaces in
// a word. No escapes, variables or globs: the words are used as they are.
// Shell punctuation outside quotes is refused so the model learns there is
// no shell instead of passing "|" as a file name.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, in = r, true
		case strings.ContainsRune("|&;<>`$", r):
			return nil, fmt.Errorf("%q: there is no shell, so pipes, redirects, ; and $ don't work; run one command", string(r))
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote")
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}

func exists(root *os.Root, p string) bool {
	c, err := Rel(p)
	if err != nil {
		return false
	}
	_, err = root.Lstat(c)
	return err == nil
}

// checkPath applies the file tools' rules to a command's path argument, plus:
// nothing under .git/ (a repository's config can make git run programs), and
// every existing part of the path must resolve inside the root through
// os.Root, so a symlink can't point a command outside it.
func checkPath(root *os.Root, p string, writes bool) error {
	c, err := Rel(p)
	if err != nil {
		return err
	}
	if InGitDir(c) {
		return fmt.Errorf("%s: paths inside .git are off limits", p)
	}
	for q := c; ; q = filepath.Dir(q) {
		_, err := root.Stat(q)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %v", p, err)
		}
		if q == "." {
			break
		}
	}
	if writes {
		if info, err := root.Lstat(c); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", p)
		}
	}
	return nil
}

// InGitDir reports whether a clean relative path is inside a .git directory.
func InGitDir(p string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(p), "/"), ".git")
}

// confirmRun asks before commands that change files.
func confirmRun(root *os.Root, raw json.RawMessage) string {
	c, err := prepare(root, raw)
	if err != nil || !c.spec.approve {
		return ""
	}
	return fmt.Sprintf("run: %s?", display(c))
}

func display(c checked) string {
	parts := []string{filepath.Base(c.prog)}
	if c.spec.git {
		parts = append(parts, c.spec.gitSub)
	}
	for _, a := range c.argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'") {
			a = fmt.Sprintf("%q", a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

func run(ctx context.Context, root *os.Root, raw json.RawMessage) (string, error) {
	c, err := prepare(root, raw)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return "", err
	}
	argv := c.argv
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "LANG=en_US.UTF-8", "TMPDIR=" + dir}
	if c.spec.git {
		argv = gitArgv(c.spec, argv)
		env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
			"GIT_OPTIONAL_LOCKS=0", "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
	}
	return Sandboxed(ctx, dir, env, c.prog, argv...)
}

// gitArgv puts the hardening options before the subcommand: no hooks, no
// fsmonitor program, no pager, no signing, a fixed identity for commits.
func gitArgv(sp spec, args []string) []string {
	out := []string{"--no-pager",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.pager=cat",
		"-c", "commit.gpgsign=false", "-c", "user.name=gostafson", "-c", "user.email=gostafson@localhost",
		"-c", "protocol.allow=never", "-c", "init.defaultBranch=main", "-c", "advice.detachedHead=false", sp.gitSub}
	out = append(out, sp.prepend...)
	return append(out, args...)
}

// SandboxExec is macOS's sandbox runner. Without it, run refuses to start
// anything.
const SandboxExec = "/usr/bin/sandbox-exec"

// sandboxProfile allows reads of the system; under the private directories
// it allows only metadata (stat, which git and ls need for the root's
// parents), not file contents or directory listings, except in the root.
// Writes only inside the root; no network. Later rules win, but a rule
// naming an operation outranks one using a wildcard, so the root's allow
// names the denied operations too.
func sandboxProfile(dir string, private []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny network*)\n(deny file-write*)\n")
	fmt.Fprintf(&b, "(allow file-write* (subpath %q) (literal \"/dev/null\"))\n", dir)
	for _, p := range private {
		fmt.Fprintf(&b, "(deny file-read-data file-read-xattr (subpath %q))\n", p)
	}
	fmt.Fprintf(&b, "(allow file-read* file-read-data file-read-xattr (subpath %q))\n", dir)
	return b.String()
}

// privateDirs are unreadable to commands: every home directory, including the
// agent's (its secrets file lives there), except the root itself.
var privateDirs = []string{"/Users"}

// Sandboxed runs prog in dir under the sandbox and returns its combined
// output, capped at MaxOutput, with the exit status appended when it isn't 0.
func Sandboxed(ctx context.Context, dir string, env []string, prog string, args ...string) (string, error) {
	if _, err := os.Stat(SandboxExec); err != nil {
		return "", errors.New("sandbox-exec is not available; commands are disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, SandboxExec, append([]string{"-p", sandboxProfile(dir, privateDirs), prog}, args...)...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, nil
	var out capped
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := out.String()
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("stopped after %s", runTimeout)
	}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		res += fmt.Sprintf("\n[exit status %d]", exit.ExitCode())
	case err != nil:
		return res, err
	}
	if res == "" {
		res = "done; no output"
	}
	return res, nil
}

// capped keeps the first MaxOutput bytes written to it.
type capped struct {
	buf   bytes.Buffer
	extra int
}

func (c *capped) Write(p []byte) (int, error) {
	room := MaxOutput - c.buf.Len()
	if room >= len(p) {
		c.buf.Write(p)
	} else {
		c.buf.Write(p[:max(room, 0)])
		c.extra += len(p) - max(room, 0)
	}
	return len(p), nil
}

func (c *capped) String() string {
	if c.extra > 0 {
		return fmt.Sprintf("%s\n[truncated: %d more bytes]", c.buf.String(), c.extra)
	}
	return c.buf.String()
}
