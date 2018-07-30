package bingen

// TODO:
// - ignore patterns

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

const Usage = `binmap: generate a golang map from binary files

Usage: bingen [options] <input>...

Inputs:

The <input> argument is a list of files and/or folders to add to the binmap.
Each entry can be prefixed with an alias which determines its output path in
the resulting map, for example 'foo:path/to/stuff' would make
'path/to/stuff/thing.jpg' available at 'foo/thing.jpg'

Compression:

If you are packing a huge amount of stuff into your map, it might get quite
large. In this case, it can be beneficial to compress the file data in the code
and unpack it all either on startup or as needed. Pass '-gzip=<level>' to 
enable compression. The compression is indiscriminate - it applies to all files
even if they're not very compressible (like jpegs).

Output formats:

Files are output as C-style byte arrays by default, which is very fast at build
time, start time and run time but the resulting go file is massive if you have
a lot of statics. Use '-b64' to emit the files as base64-encoded strings instead.
The output is hideous, but the output isn't really meant for humans to read.
`

type usageError string

func (u usageError) Error() string { return string(u) }

func IsUsageError(err error) bool {
	_, ok := err.(usageError)
	return ok
}

type Command struct {
	out    string
	pkg    string
	name   string
	b64    bool
	nofmt  bool
	gzip   int
	ignore stringList
}

func (m *Command) Flags(fs *flag.FlagSet) {
	fs.StringVar(&m.out, "out", "", "")
	fs.StringVar(&m.pkg, "pkg", "", "")
	fs.StringVar(&m.name, "name", "files", "")
	fs.BoolVar(&m.b64, "b64", false, "")
	fs.BoolVar(&m.nofmt, "nofmt", false, "")
	fs.IntVar(&m.gzip, "gzip", 0, "gzip compression level (0 for none)")
	fs.Var(&m.ignore, "ignore", "regexp patterns to ignore")
}

func (m *Command) Usage() string { return Usage }

func (m *Command) Run(args ...string) error {
	if len(args) == 0 {
		return usageError("binmap: missing <input> argument(s)")
	}

	if m.pkg == "" {
		m.pkg = os.Getenv("GOPACKAGE")
	}
	if m.pkg == "" {
		return usageError("binmap: must specify package using -pkg or $GOPACKAGE")
	}

	var buf bytes.Buffer

	inputs, err := readInputs(args)
	if err != nil {
		return err
	}

	var ignore []*regexp.Regexp
	for _, ig := range m.ignore {
		ptn, err := regexp.Compile(ig)
		if err != nil {
			return fmt.Errorf("binmap: could not compile pattern %s: %v", ig, err)
		}
		ignore = append(ignore, ptn)
	}

	names, files, err := loadFiles(inputs, ignore)
	if err != nil {
		return err
	}

	if m.gzip > 0 {
		if err := gzipFiles(files, m.gzip); err != nil {
			return err
		}
	}

	// Generate output map
	var fileData bytes.Buffer
	if m.b64 {
		if err := writeFilesAsBase64(&fileData, names, files); err != nil {
			return err
		}
	} else {
		if err := writeFilesAsByteArray(&fileData, names, files); err != nil {
			return err
		}
	}

	// Create source file
	tpl := template.Must(template.New("").Parse(binMapTpl))
	err = tpl.Execute(&buf, struct {
		Package  string
		Name     string
		Map      string
		Deflated bool
	}{Package: m.pkg, Name: m.name, Map: fileData.String(), Deflated: m.gzip != 0})
	if err != nil {
		return err
	}

	// Format output
	var p []byte
	if m.nofmt {
		p = buf.Bytes()
	} else {
		p, err = format.Source(buf.Bytes())
		if err != nil {
			return err
		}
	}

	var writer io.Writer
	if m.out == "" {
		writer = os.Stdout
	} else {
		if fileExists(m.out) {
			contents, err := ioutil.ReadFile(m.out)
			if err != nil {
				return err
			}
			if bytes.Equal(contents, p) {
				fmt.Fprintf(os.Stderr, "binmap: unmodified %v\n", names)
				return nil
			}
		}
		writer, err = os.OpenFile(m.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
	}
	_, err = writer.Write(p)
	if err != nil {
		return err
	}

	if wc, ok := writer.(io.WriteCloser); ok && wc != os.Stdout {
		err = wc.Close()
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "binmap: created %v\n", names)
	}

	return nil
}

func writeFilesAsBase64(into *bytes.Buffer, names []string, files map[string][]byte) error {
	into.WriteString("map[string]string{\n")

	for _, name := range names {
		var buf bytes.Buffer
		wrt := base64.NewEncoder(base64.StdEncoding, &buf)
		wrt.Write(files[name])
		wrt.Close()
		val := wrap(buf.String(), 100)
		into.WriteString(fmt.Sprintf("%q: `%s`,\n\n", name, val))
	}

	into.WriteString("}\n")
	return nil
}

func writeFilesAsByteArray(into *bytes.Buffer, names []string, files map[string][]byte) error {
	into.WriteString("map[string][]byte{\n")

	for _, name := range names {
		into.WriteString(fmt.Sprintf("%q: []byte{\n", name))
		for i, b := range files[name] {
			if i > 0 && i%16 == 0 {
				into.WriteByte('\n')
			}
			into.WriteString(fmt.Sprintf("0x%02x, ", b))
		}
		into.WriteString("},\n")
	}

	into.WriteString("}\n")
	return nil
}

func gzipFiles(files map[string][]byte, level int) error {
	for n, data := range files {
		var buf bytes.Buffer
		err := func() error {
			fw, err := gzip.NewWriterLevel(&buf, level)
			if err != nil {
				return err
			}
			defer func() {
				if err := fw.Close(); err != nil {
					panic(err)
				}
			}()
			if _, err := fw.Write(data); err != nil {
				return err
			}
			return nil
		}()

		if err != nil {
			return err
		}
		files[n] = buf.Bytes()
	}
	return nil
}

func loadFiles(in []input, ignore []*regexp.Regexp) (names []string, files map[string][]byte, err error) {
	files = make(map[string][]byte)
	names = make([]string, 0)

	addFile := func(base input, src string) error {
		for _, ig := range ignore {
			if ig.MatchString(src) {
				return nil
			}
		}

		src = filepath.ToSlash(src)

		key := src
		if base.Alias != "" {
			if strings.HasPrefix(src, base.Path) {
				key = base.Alias + "/" + strings.TrimLeft(key[len(base.Path):], "/")
			}
		}

		files[key], err = ioutil.ReadFile(filepath.FromSlash(src))
		if err != nil {
			return err
		}
		names = append(names, key)
		return nil
	}

	for _, src := range in {
		var fi os.FileInfo
		fi, err = os.Stat(src.Path)
		if err != nil {
			return
		}
		if fi.IsDir() {
			err = filepath.Walk(src.Path, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					if err = addFile(src, path); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return
			}
		} else {
			if err = addFile(src, src.Path); err != nil {
				return
			}
		}
	}
	sort.Strings(names)
	return
}

type input struct {
	Alias string
	Path  string
}

func readInputs(in []string) (r []input, err error) {
	for _, v := range in {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) == 1 {
			r = append(r, input{"", filepath.ToSlash(parts[0])})
		} else {
			r = append(r, input{parts[0], filepath.ToSlash(parts[1])})
		}
	}
	return
}

func fileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func wrap(s string, w int) string {
	var buf strings.Builder
	l := len(s)
	for l >= w {
		buf.WriteString(s[0:w])
		buf.WriteByte('\n')
		s = s[w:]
		l -= w
	}
	if l > 0 {
		buf.WriteString(s)
	}
	return buf.String()
}

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s stringList) Strings() []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

var binMapTpl = `
// This code was autogenerated from "binmap"
// DO NOT MODIFY THIS FILE DIRECTLY!
// Your changes will be overwritten!

{{ if .Deflated }}// File data is compressed! See compress/gzip.{{ end }}

package {{.Package}}

var {{.Name}} = {{.Map}}
`
