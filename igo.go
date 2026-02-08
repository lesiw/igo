package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/shlex"
	"golang.org/x/tools/imports"

	"lesiw.io/defers"
)

var builderr = regexp.MustCompile(`^(\./[^\s:]+):(\d+):(\d+):\s*(.+)$`)
var errEOF = errors.New("bad EOF")

const unused = "declared and not used: "
const foundEOF = "found 'EOF'"

type session struct {
	dir string       // Working directory.
	pth string       // Path to source file.
	src []byte       // Source code.
	off int          // Offset to the last bracket of main().
	frm int          // Last printed line.
	usr bytes.Buffer // User code.
	rem string       // Remaining output after EOF.
}

func main() {
	defer defers.Run()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		defers.Exit(1)
	}
}

func run() error {
	var s session
	var err error
	if len(os.Args) < 2 {
		dir, err := os.MkdirTemp("", "igo")
		if err != nil {
			return fmt.Errorf("failed to create temporary directory: %w", err)
		}
		defers.Add(func() { _ = os.RemoveAll(dir) })
		cmd := exec.Command("go", "mod", "init", "igo.localhost")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf(`failed to run "go mod init": %s`,
				bytes.TrimSpace(out))
		}
		s.pth = filepath.Join(dir, "main.go")
		s.src = []byte("package main\n\nfunc main() {}\n")
		s.off = len(s.src) - 2
		s.dir = dir
	} else {
		s.pth = os.Args[1]
		s.src, err = os.ReadFile(s.pth)
		if err != nil {
			return fmt.Errorf("bad file %q: %w", s.pth, err)
		}
		src := []byte(s.src)
		defers.Add(func() { _ = os.WriteFile(s.pth, src, 0644) })
		if err := s.prepareSrc(); err != nil {
			return err
		}
	}
	return s.run()
}

func (s *session) prepareSrc() error {
	fs := token.NewFileSet()
	root, err := parser.ParseFile(fs, filepath.Base(s.pth), s.src,
		parser.AllErrors)
	if err != nil {
		return fmt.Errorf("failed to parse: %d", err)
	}
	if root.Name.Name != "main" {
		// Set to package main.
		root.Name.Name = "main"
		var buf bytes.Buffer
		if err := format.Node(&buf, fs, root); err != nil {
			return fmt.Errorf("failed to modify source: %w", err)
		}
		s.src = buf.Bytes()
	}
	var found bool
	ast.Inspect(root, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			return true
		}
		found = true
		s.off = fs.Position(fn.Body.Rbrace).Offset - 1
		return true
	})
	if !found {
		s.src = append(s.src, []byte("\n\nfunc main() {}\n")...)
		s.off = len(s.src) - 2
	}
	return nil
}

func (s *session) run() error {
	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		var line string
	read:
		input, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}
		input = strings.TrimSpace(input)
		if input == ".quit" || input == ".exit" {
			fmt.Print(s.rem)
			break
		}
		if strings.HasPrefix(input, ":") {
			argv, err := shlex.Split(input[1:])
			if err != nil || len(argv) == 0 {
				fmt.Fprintf(os.Stderr, "bad command: %s", err)
				continue
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir = s.dir
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "command failed: ")
				if ee := new(exec.ExitError); errors.As(err, &ee) {
					fmt.Fprintf(os.Stderr, "%s\n",
						bytes.TrimSuffix(out, []byte("\n")))
				} else {
					fmt.Fprintf(os.Stderr, "%s\n", err)
				}
			}
		} else {
			line += input
			if err := s.exec(line); errors.Is(err, errEOF) {
				goto read
			} else if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}
	return nil
}

func (s *session) exec(input string) error {
	var fixes strings.Builder
	input = input + "\n"
rerun:
	if err := s.write(input + fixes.String()); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	buf, err := imports.Process(s.pth, nil, nil)
	if err != nil && strings.Contains(err.Error(), foundEOF) {
		return errEOF
	} else if err != nil {
		return fmt.Errorf("failed to process imports: %w", err)
	}
	if err := os.WriteFile(s.pth, buf, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	cmd := exec.Command("go", "run", s.pth)
	cmd.Dir = s.dir
	buf, err = cmd.CombinedOutput()
	output := string(buf)
	if err != nil {
		lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
		if strings.HasPrefix(lines[len(lines)-1], "exit status ") {
			// The program errored, so return its error.
			return errors.New(strings.TrimSuffix(s.newLines(output), "\n"))
		}
		// This is a compile error, so try to fix it.
		var fixed bool
		for line := range strings.SplitSeq(output, "\n") {
			if m := builderr.FindStringSubmatch(line); m != nil {
				if strings.HasPrefix(m[4], unused) {
					fixed = true
					fixes.WriteString("_ = " + m[4][len(unused):] + "\n")
				}
			}
		}
		if fixed {
			goto rerun
		}
		return errors.New(strings.TrimSuffix(output, "\n"))
	}
	s.usr.WriteString(input)
	out := strings.TrimSuffix(s.newLines(output), "\n")
	if out != "" {
		fmt.Println(out)
	}
	s.frm += strings.Count(s.newLines(output), "\n")
	return nil
}

func (s *session) write(input string) (err error) {
	f, err := os.Create(s.pth)
	if err != nil {
		return err
	}
	defer f.Close()
	w := func(b []byte) {
		if err != nil {
			return
		}
		_, err = f.Write(b)
	}
	w(s.src[:s.off])
	w([]byte("\n"))
	w(s.usr.Bytes())
	w([]byte(input))
	w([]byte(`println("\000igo:EOF")`))
	w(s.src[s.off:])
	return
}

func (s *session) newLines(output string) string {
	start := len(output)
	var count int
	for i, r := range output {
		if count >= s.frm {
			start = i
			break
		}
		if r == '\n' {
			count++
		}
	}
	const eof = "\000igo:EOF\n"
	end := strings.Index(output, eof)
	if end < start {
		end = len(output)
	}
	if n := end + len(eof); n < len([]rune(output)) {
		s.rem = strings.TrimSuffix(string([]rune(output)[n:]), "\n") + "\n"
	} else {
		s.rem = ""
	}
	return string([]rune(output)[start:end])
}
