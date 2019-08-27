package bingen

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

type Mode string

func (m Mode) String() string { return string(m) }

func (m *Mode) Set(s string) error {
	v := Mode(s)
	if v != Base64 && v != Bytes {
		return fmt.Errorf("bingen: invalid mode %q", s)
	}
	*m = v
	return nil
}

const (
	Base64 Mode = "base64"
	Bytes  Mode = "bytes"
)

type Command struct {
	out    string
	pkg    string
	name   string
	mode   Mode
	nofmt  bool
	rawMap bool
	gzip   int
	ignore stringList
	tags   string
}

func (m *Command) Flags(fs *flag.FlagSet) {
	fs.StringVar(&m.out, "out", "", "Output file")
	fs.StringVar(&m.pkg, "pkg", "", "Output package (uses the GOPACKAGE env var if empty)")
	fs.StringVar(&m.name, "name", "files", "Output variable name")
	fs.Var(&m.mode, "mode", "Encode mode (base64, bytes)")
	fs.StringVar(&m.tags, "tags", "", "Build tags")
	fs.BoolVar(&m.nofmt, "nofmt", false, "Do not run gofmt after generation")
	fs.BoolVar(&m.rawMap, "rawmap", false, "Use a raw map instead of a Config")
	fs.IntVar(&m.gzip, "gzip", 9, "gzip compression level (0 for none)")
	fs.Var(&m.ignore, "ignore", "regexp pattern to ignore. Can pass multiple times.")
}

func (m *Command) Synopsis() string { return "Embed binary files into Go source" }
func (m *Command) Usage() string    { return Usage }

func (m *Command) Run(args ...string) (rerr error) {
	if len(args) == 0 {
		return usageError("binmap: missing <input> argument(s)")
	}
	if m.pkg == "" {
		m.pkg = os.Getenv("GOPACKAGE")
	}
	if m.pkg == "" {
		return usageError("binmap: must specify package using -pkg or $GOPACKAGE")
	}
	if m.out == "" {
		return usageError("binmap: must specify output using -out")
	}
	if m.mode == "" {
		m.mode = Base64
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
	{
		var err error
		switch m.mode {
		case Base64:
			err = writeFilesAsBase64(&fileData, names, files)
		case Bytes:
			err = writeFilesAsByteArray(&fileData, names, files)
		default:
			err = fmt.Errorf("unknown mode %q", m.mode)
		}
		if err != nil {
			return err
		}
	}

	// Create source file
	tpl := template.Must(template.New("").Parse(binMapTpl))
	err = tpl.Execute(&buf, &binMapVars{
		Package:  m.pkg,
		Name:     m.name,
		Tags:     m.tags,
		Map:      strings.TrimSpace(fileData.String()),
		Deflated: m.gzip != 0,
		Mode:     string(m.mode),
		AsConfig: !m.rawMap,
	})
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

	if exists, err := fileExists(m.out); err != nil {
		return err

	} else if exists {
		if mod, err := isModified(m.out, p); err != nil {
			return err
		} else if !mod {
			fmt.Fprintf(os.Stderr, "binmap: unmodified %v\n", names)
			return nil
		}
	}

	f, err := os.OpenFile(m.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Write(p); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "binmap: created %v\n", names)

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

func isModified(file string, orig []byte) (bool, error) {
	contents, err := ioutil.ReadFile(file)
	if err != nil {
		return false, err
	}
	if bytes.Equal(contents, orig) {
		return false, nil
	}
	return true, nil
}

func loadFiles(inputs []input, ignore []*regexp.Regexp) (names []string, files map[string][]byte, err error) {
	files = make(map[string][]byte)
	names = make([]string, 0)

	addFile := func(base input, src string, isDir bool) error {
		for _, ig := range ignore {
			if ig.MatchString(src) {
				return nil
			}
		}

		src = filepath.ToSlash(src)

		key := strings.TrimLeft(src, "/")
		parts := strings.Split(key, "/")
		if len(parts) < base.Strip {
			return fmt.Errorf("path shorter than strip")
		}
		parts = parts[base.Strip:]
		key = strings.Join(parts, "/")

		if base.Alias != "" {
			key = base.Alias + "/" + key
		}

		files[key], err = ioutil.ReadFile(filepath.FromSlash(src))
		if err != nil {
			return err
		}
		names = append(names, key)
		return nil
	}

	for _, src := range inputs {
		var fi os.FileInfo
		fi, err := os.Stat(src.Path)
		if err != nil {
			return nil, nil, err
		}

		if fi.IsDir() {
			if err := filepath.Walk(src.Path, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					if err = addFile(src, path, true); err != nil {
						return err
					}
				}
				return nil

			}); err != nil {
				return nil, nil, err
			}

		} else if err := addFile(src, src.Path, false); err != nil {
			return nil, nil, err
		}
	}

	sort.Strings(names)
	return
}

type input struct {
	Alias string
	Strip int
	Path  string
}

func readInputs(in []string) (r []input, err error) {
	for _, v := range in {
		parts := strings.SplitN(v, ":", 3)
		if len(parts) == 1 {
			r = append(r, input{"", 0, filepath.ToSlash(parts[0])})

		} else if len(parts) == 2 {
			r = append(r, input{parts[0], 0, filepath.ToSlash(parts[1])})

		} else {
			split, err := strconv.ParseInt(parts[1], 10, 0)
			if err != nil {
				return nil, fmt.Errorf("cannot parse split: %v", err)
			}
			r = append(r, input{parts[0], int(split), filepath.ToSlash(parts[2])})
		}
	}

	return r, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
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

type binMapVars struct {
	Package  string
	Name     string
	Tags     string
	Map      string
	Deflated bool
	AsConfig bool
	Mode     string
}

var binMapTpl = `
// Code generated by 'github.com/shabbyrobe/go-bingen'. DO NOT EDIT.

{{ if .Deflated }}// File data is compressed! See compress/gzip.{{ end }}

{{ if .Tags }}// +build {{.Tags}}{{ end }}

package {{.Package}}

{{ if .AsConfig }}
import "github.com/shabbyrobe/go-bingen/binfs"

var {{.Name}} = binfs.Config{
	Gzip: {{ if .Deflated }}true{{ else }}false{{ end }},
	Mode: {{printf "%q" .Mode}},
	Data: {{.Map -}},
}
{{ else }}
var {{.Name}} = {{.Map}}
{{ end }}
`
