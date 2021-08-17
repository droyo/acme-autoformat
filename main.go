package main

import (
	"bufio"
	"bytes"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"9fans.net/go/acme"
)

var (
	filePattern = flag.String("r", "", "Pattern to match source files")
	addressRe   = regexp.MustCompile(
		`^@@\s*-(?P<os>\d+),(?P<oe>\d+)\s+\+(?P<ns>\d+),(?P<ne>\d+)`)
)

type params struct {
	Basename, Dirname, Fullname string // the names of the file
}

func main() {
	flag.Parse()
	log.SetFlags(0)

	filenameRe, err := regexp.Compile(*filePattern)
	if err != nil {
		log.Fatal("invalid pattern: ", err)
	}

	if flag.NArg() < 1 {
		log.Fatal("usage: acme-ocamlformat [-r pat] -- command ...")
	}
	l, err := acme.Log()
	if err != nil {
		log.Fatal(err)
	}

	argsExpanded := make([]string, flag.NArg())
	argsTemplate := make([]*template.Template, 0, flag.NArg())

	for _, s := range flag.Args() {
		tmpl, err := template.New("arg").Parse(s)
		if err != nil {
			log.Fatal(err)
		}
		argsTemplate = append(argsTemplate, tmpl)
	}

	var buf bytes.Buffer
Loop:
	for {
		event, err := l.Read()
		if err != nil {
			log.Fatal(err)
		}

		if event.Op == "put" && filenameRe.MatchString(event.Name) {
			p := params{
				Basename: filepath.Base(event.Name),
				Dirname:  filepath.Dir(event.Name),
				Fullname: event.Name,
			}
			argsExpanded = argsExpanded[:0]
			for _, tmpl := range argsTemplate {
				buf.Reset()
				if err := tmpl.Execute(&buf, &p); err != nil {
					log.Print(err)
					continue Loop
				}
				argsExpanded = append(argsExpanded, buf.String())
			}
			autoFormat(event.ID, event.Name, argsExpanded)
		}
	}
}

func autoFormat(id int, name string, args []string) {
	var fmtErrBuf bytes.Buffer

	w, err := acme.Open(id, nil)
	if err != nil {
		log.Print(err)
		return
	}
	defer w.CloseFiles()

	format := exec.Command(args[0], args[1:]...)
	diff := exec.Command("diff", "-u", "/dev/fd/3", "/dev/fd/0")

	body, err := w.ReadAll("body")
	if err != nil {
		log.Fatal(err)
	}
	rbody, wbody, err := os.Pipe()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		io.Copy(wbody, bytes.NewReader(body))
		wbody.Close()
	}()

	// Get formatter/diff errors to show up in a window sharing
	// the file's path, so addresses in the errors can be plumbed
	w.SetErrorPrefix(name)

	diff.Stdin, _ = format.StdoutPipe()
	diff.ExtraFiles = append(diff.ExtraFiles, rbody)

	format.Stdin = bytes.NewReader(body)
	format.Stderr = &fmtErrBuf

	if err := format.Start(); err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := format.Wait(); err != nil {
			if _, ok := err.(*exec.ExitError); ok {
				w.Err(strings.Join(format.Args, " "))
				w.Err(fmtErrBuf.String())
			} else {
				w.Err(err.Error())
			}
		}
	}()

	output, err := diff.CombinedOutput()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			// From diff(1): Exit status is 0 if inputs are the same, 1 if different, 2 if trouble.
			switch exit.ExitCode() {
			case 1:
				applyPatch(bytes.NewReader(output), w)
			default:
				w.Err(strings.Join(diff.Args, " "))
				w.Err(string(output))
			}
		} else {
			w.Err(err.Error())
		}
	}
}

// Applies a unified diff to the acme window, line by line
func applyPatch(patch io.Reader, w *acme.Win) {
	// The following changes are grouped together as a single logical change,
	// and Undo will undo all of them.
	w.Ctl("nomark")
	w.Ctl("mark")

	scanner := bufio.NewScanner(patch)
	for i := 0; i < 2; i++ {
		if !scanner.Scan() {
			println("invalid diff")
			return
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case addressRe.MatchString(line):
			// hunk header; jumps to start of the new line; since we're editing top to bottom,
			// the new line number ($ns) represents the start of the hunk in the window
			submatch := addressRe.FindStringSubmatchIndex(line)
			w.Addr("%s-1+0", addressRe.ExpandString(nil, "$ns", line, submatch))
		case strings.HasPrefix(line, "-"):
			// start building a range over lines to remove. the next write of a new or
			// unchanged line will remove selected lines.
			w.Addr(".,+")
		case strings.HasPrefix(line, " "):
			// writing nothing here has the side effect of removing any selected lines
			w.Write("data", nil)
			w.Addr("+1+0")
		case strings.HasPrefix(line, "+"):
			w.Write("data", scanner.Bytes()[1:])
			// scanner does not include the delimiting newlines.
			w.Write("data", []byte("\n"))
		case strings.HasPrefix(line, `\\`):
			continue
		case line == "":
			w.Write("data", nil)
		default:
			log.Printf("don't know how to parse diff line %q", line)
			return
		}
		//w.Ctl("dot=addr")
		//time.Sleep(time.Second * 1)
	}

	if scanner.Err() != nil {
		log.Fatal(scanner.Err())
	}
}
